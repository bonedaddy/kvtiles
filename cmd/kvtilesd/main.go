package main

import (
	"context"
	"encoding/json"
	"fmt"
	stdlog "log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	// _ "net/http/pprof"

	log "github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/namsral/flag"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metrics "github.com/slok/go-http-metrics/metrics/prometheus"
	"github.com/slok/go-http-metrics/middleware"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/akhenakh/kvtiles/loglevel"
	"github.com/akhenakh/kvtiles/server"
	"github.com/akhenakh/kvtiles/storage/bbolt"
)

const appName = "kvtilesd"

var (
	version = "no version from LDFLAGS"

	logLevel        = flag.String("logLevel", "INFO", "DEBUG|INFO|WARN|ERROR")
	dbPath          = flag.String("dbPath", "map.db", "Database path")
	httpMetricsPort = flag.Int("httpMetricsPort", 8088, "http port")
	httpAPIPort     = flag.Int("httpAPIPort", 9201, "http API port")
	healthPort      = flag.Int("healthPort", 6666, "grpc health port")

	httpServer        *http.Server
	grpcHealthServer  *grpc.Server
	httpMetricsServer *http.Server

	templatesNames = []string{"osm-liberty-gl.style", "planet.json", "index.html", "mapbox.html"}
)

func main() {
	flag.Parse()

	logger := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	logger = log.With(logger, "caller", log.Caller(5), "ts", log.DefaultTimestampUTC)
	logger = log.With(logger, "app", appName)
	logger = loglevel.NewLevelFilterFromString(logger, *logLevel)

	stdlog.SetOutput(log.NewStdlibAdapter(logger))

	level.Info(logger).Log("msg", "Starting app", "version", version)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// catch termination
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	g, ctx := errgroup.WithContext(ctx)

	// pprof
	// go func() {
	// 	stdlog.Println(http.ListenAndServe("localhost:6060", nil))
	// }()

	storage, clean, err := bbolt.NewROStorage(*dbPath, logger)
	if err != nil {
		level.Error(logger).Log("msg", "failed to open storage", "error", err, "db_path", *dbPath)
		os.Exit(2)
	}
	defer clean()

	infos, ok, err := storage.LoadMapInfos()
	if err != nil {
		level.Error(logger).Log("msg", "failed to read infos", "error", err)
		os.Exit(2)
	}
	if !ok {
		level.Error(logger).Log("msg", "no map infos")
		os.Exit(2)
	}

	// gRPC Health Server
	healthServer := health.NewServer()
	g.Go(func() error {
		grpcHealthServer = grpc.NewServer()

		healthpb.RegisterHealthServer(grpcHealthServer, healthServer)

		haddr := fmt.Sprintf(":%d", *healthPort)
		hln, err := net.Listen("tcp", haddr)
		if err != nil {
			level.Error(logger).Log("msg", "gRPC Health server: failed to listen", "error", err)
			os.Exit(2)
		}
		level.Info(logger).Log("msg", fmt.Sprintf("gRPC health server listening at %s", haddr))
		return grpcHealthServer.Serve(hln)
	})

	// server
	server, err := server.New(storage, logger, healthServer)
	if err != nil {
		level.Error(logger).Log("msg", "can't get a working server", "error", err)
		os.Exit(2)
	}

	// web server metrics
	g.Go(func() error {
		httpMetricsServer = &http.Server{
			Addr:         fmt.Sprintf(":%d", *httpMetricsPort),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		level.Info(logger).Log("msg", fmt.Sprintf("HTTP Metrics server listening at :%d", *httpMetricsPort))

		versionGauge.WithLabelValues(version).Add(1)
		dataVersionGauge.WithLabelValues(
			fmt.Sprintf("%s %s", infos.Region, infos.IndexTime.Format(time.RFC3339)),
		).Add(1)

		// Register Prometheus metrics handler.
		http.Handle("/metrics", promhttp.Handler())

		if err := httpMetricsServer.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}

		return nil
	})

	// web server
	g.Go(func() error {
		// metrics middleware.
		metricsMwr := middleware.New(middleware.Config{
			Recorder: metrics.NewRecorder(metrics.Config{Prefix: appName}),
		})

		r := mux.NewRouter()

		r.Handle("/tiles/{z}/{x}/{y}", metricsMwr.Handler("/tiles/", server))

		// static file handler
		fileHandler := http.FileServer(http.Dir("./static"))

		// computing templates
		pathTpls := make([]string, len(templatesNames))
		for i, name := range templatesNames {
			pathTpls[i] = "./static/" + name
		}
		t, err := template.ParseFiles(pathTpls...)
		if err != nil {
			level.Error(logger).Log("msg", "can't parse templates", "error", err)
			os.Exit(2)
		}

		// serving templates and static files
		r.PathPrefix("/static/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/static/")
			if path == "" {
				path = "index.html"
			}

			// serve file normally
			if !isTpl(path) {
				r.URL.Path = path
				fileHandler.ServeHTTP(w, r)
				return
			}

			mapInfos, ok, err := storage.LoadMapInfos()
			if err != nil {
				http.Error(w, err.Error(), 500)
				level.Error(logger).Log("msg", "error reading db", "error", err)
				return
			}
			if !ok {
				http.Error(w, "no map in DB", 404)
				level.Error(logger).Log("msg", "db does not contain a map")
				return
			}

			// Templates variables
			proto := "http"
			if r.Header.Get("X-Forwarded-Proto") == "https" {
				proto = "https"
			}

			p := map[string]interface{}{
				"TilesBaseURL": fmt.Sprintf("%s://%s", proto, r.Host),
				"MaxZoom":      mapInfos.MaxZoom,
				"CenterLat":    mapInfos.CenterLat,
				"CenterLng":    mapInfos.CenterLng,
			}

			// change header base on content-type
			ctype := mime.TypeByExtension(filepath.Ext(path))
			w.Header().Set("Content-Type", ctype)

			err = t.ExecuteTemplate(w, path, p)
			if err != nil {
				http.Error(w, err.Error(), 500)
				level.Error(logger).Log("msg", "can't execute template", "error", err, "path", path)
				return
			}
		})

		r.HandleFunc("/healthz", func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
			defer cancel()

			resp, err := healthServer.Check(ctx, &healthpb.HealthCheckRequest{
				Service: fmt.Sprintf("grpc.health.v1.%s", appName)},
			)
			if err != nil {
				json := []byte(fmt.Sprintf("{\"status\": \"%s\"}", healthpb.HealthCheckResponse_UNKNOWN.String()))
				w.WriteHeader(http.StatusInternalServerError)
				w.Write(json)
				return
			}
			if resp.Status != healthpb.HealthCheckResponse_SERVING {
				w.WriteHeader(http.StatusInternalServerError)
			}
			json := []byte(fmt.Sprintf("{\"status\": \"%s\"}", resp.Status.String()))
			w.Write(json)
		})

		r.HandleFunc("/version", func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			m := map[string]interface{}{"version": version, "infos": infos}
			b, _ := json.Marshal(m)
			w.Write(b)
		})

		httpServer = &http.Server{
			Addr:         fmt.Sprintf(":%d", *httpAPIPort),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			Handler:      handlers.CORS()(r),
		}
		level.Info(logger).Log("msg", fmt.Sprintf("HTTP API server listening at :%d", *httpAPIPort))

		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}

		return nil
	})

	healthServer.SetServingStatus(fmt.Sprintf("grpc.health.v1.%s", appName), healthpb.HealthCheckResponse_SERVING)
	level.Info(logger).Log("msg", "serving status to SERVING")

	select {
	case <-interrupt:
		cancel()
		break
	case <-ctx.Done():
		break
	}

	level.Warn(logger).Log("msg", "received shutdown signal")

	healthServer.SetServingStatus(fmt.Sprintf("grpc.health.v1.%s", appName), healthpb.HealthCheckResponse_NOT_SERVING)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if httpMetricsServer != nil {
		_ = httpMetricsServer.Shutdown(shutdownCtx)
	}

	if httpServer != nil {
		_ = httpServer.Shutdown(shutdownCtx)
	}

	if grpcHealthServer != nil {
		grpcHealthServer.GracefulStop()
	}

	err = g.Wait()
	if err != nil {
		level.Error(logger).Log("msg", "server returning an error", "error", err)
		os.Exit(2)
	}
}

func isTpl(path string) bool {
	for _, p := range templatesNames {
		if p == path {
			return true
		}
	}
	return false
}