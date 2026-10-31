package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"user-service/internal/users"
	pb "user-service/proto"

	"github.com/grafana/pyroscope-go"
	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

const serviceName = "user-service"

var log *logrus.Logger

func init() {
	log = logrus.New()
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout
}

func main() {
	port := "8086"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	// `user-service healthcheck` is the container health probe: exit 0 only while the server reports SERVING.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(port))
	}

	// --- Telemetry: traces, metrics and logs pushed over OTLP/gRPC (Alloy → Tempo/Mimir/Loki) ---
	shutdownTelemetry, err := initTelemetry(context.Background(), serviceName, log)
	switch {
	case errors.Is(err, errTelemetryOff):
		log.Info("telemetry off — set OTEL_EXPORTER_OTLP_ENDPOINT to enable")
	case err != nil:
		log.Warnf("telemetry disabled, continuing without it: %v", err)
	default:
		log.Info("telemetry enabled (OTLP/gRPC)")
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(ctx)
		}()
	}

	// --- Profiling (Pyroscope push + pprof HTTP), only with PYROSCOPE_ADDR set and ENABLE_PROFILING != 0 ---
	pyroscopeAddr, profiling := profilingConfig()
	if profiling {
		initProfiling(pyroscopeAddr)
	} else {
		log.Info("profiling off — set PYROSCOPE_ADDR to enable")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	if err := run(port, dsn, profiling); err != nil {
		log.Errorf("server error: %v", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

func run(port, dsn string, profiling bool) error {
	cfg0, err := configFromEnv(os.Getenv, log)
	if err != nil {
		return err
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err) // the error never contains the password
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg) // lazy: does not connect yet
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer pool.Close()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %s: %w", port, err)
	}

	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	// gRPC server with OTel tracing + Prometheus metrics interceptors, plus a panic guard
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(grpcprom.UnaryServerInterceptor, recoverUnary),
		grpc.StreamInterceptor(grpcprom.StreamServerInterceptor),
	)

	store := users.NewPGStore(pool)
	svc, err := users.NewService(store, log, cfg0)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	pb.RegisterUserServiceServer(srv, svc)

	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	setHealth(healthSrv, healthpb.HealthCheckResponse_NOT_SERVING)
	reflection.Register(srv)

	grpcprom.Register(srv)
	grpcprom.EnableHandlingTimeHistogram()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// --- Metrics (+ pprof only with ENABLE_PROFILING=1) ---
	metricsPort := "9090"
	if p := os.Getenv("METRICS_PORT"); p != "" {
		metricsPort = p
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	if profiling {
		metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
		metricsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		metricsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		metricsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	httpSrv := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      60 * time.Second, // pprof profile/trace default to 30 s
	}
	go func() {
		log.Infof("metrics endpoint on :%s (pprof: %v)", metricsPort, profiling)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warnf("metrics server error: %v", err)
		}
	}()

	// --- Database: migrate (retrying until the DB is reachable), then watch it ---
	go watchDatabase(ctx, pool, store, svc, healthSrv)

	serveErr := make(chan error, 1)
	go func() {
		log.Infof("starting grpc server on %s", listener.Addr().String())
		serveErr <- srv.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("shutdown signal received, draining")
	healthSrv.Shutdown() // NOT_SERVING for every service, so probes fail first
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Warn("graceful stop timed out, closing remaining connections")
		srv.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	svc.Wait()
	return nil
}

func setHealth(h *health.Server, st healthpb.HealthCheckResponse_ServingStatus) {
	h.SetServingStatus("", st)
	h.SetServingStatus(pb.UserService_ServiceDesc.ServiceName, st)
}

// watchDatabase runs the migrations until they succeed, then pings the database
// every few seconds and keeps the health status (and the handlers) in line with it.
func watchDatabase(ctx context.Context, pool *pgxpool.Pool, store users.Store, svc *users.Service, h *health.Server) {
	for attempt := 1; ; attempt++ {
		applied, err := users.Setup(ctx, pool)
		if err == nil {
			log.Infof("database ready: %d migration(s) applied", applied)
			break
		}
		if ctx.Err() != nil {
			return
		}
		log.Warnf("database not ready (attempt %d), retrying in 2s: %v", attempt, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	serving := false
	check := func() {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := store.Ping(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil && !serving:
			serving = true
			svc.SetReady(true)
			setHealth(h, healthpb.HealthCheckResponse_SERVING)
			log.Info("health: SERVING")
		case err != nil && serving:
			serving = false
			svc.SetReady(false)
			setHealth(h, healthpb.HealthCheckResponse_NOT_SERVING)
			log.Warnf("health: NOT_SERVING, database unreachable: %v", err)
		}
	}
	check()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// recoverUnary turns a handler panic into INTERNAL instead of crashing the process.
func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

// profilingConfig returns the Pyroscope address and whether profiling is on:
// only when PYROSCOPE_ADDR is set and non-empty and ENABLE_PROFILING is not "0".
func profilingConfig() (string, bool) {
	addr := strings.TrimSpace(os.Getenv("PYROSCOPE_ADDR"))
	if addr == "" || os.Getenv("ENABLE_PROFILING") == "0" {
		return "", false
	}
	return addr, true
}

// initProfiling starts the Pyroscope push-based profiler.
func initProfiling(pyroscopeAddr string) {

	_, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: serviceName,
		ServerAddress:   pyroscopeAddr,
		Logger:          pyroscope.StandardLogger,
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
		},
	})
	if err != nil {
		log.Warnf("pyroscope init failed, continuing without it: %v", err)
		return
	}
	log.Info("profiling enabled → pushing to " + pyroscopeAddr)
}
