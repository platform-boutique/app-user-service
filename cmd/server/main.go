package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"user-service/internal/obs"
	"user-service/internal/users"
	pb "user-service/proto"

	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

const serviceName = "user-service"

var log = logrus.New()

// drainTimeout is how long in-flight calls get after SIGTERM. With the telemetry flush bound
// (obs.DefaultShutdownTimeout, 4 s) the process exits within 9 s (CODE-04: 10 s).
const drainTimeout = 5 * time.Second

func main() {
	port := "8086"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	// `user-service healthcheck` is the container health probe: exit 0 only while the server reports SERVING.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(port))
	}

	// Logger, env contract (OTEL_*, PYROSCOPE_ADDR), OTLP pipelines, Pyroscope, build_info and
	// /metrics on :9090 live in internal/obs. pprof is served only if PPROF_ADDR is set.
	o := obs.Start(context.Background(), obs.Options{Service: serviceName, Loggers: []*logrus.Logger{log}})
	defer o.Recover()
	defer o.Shutdown()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		o.Fatal("DATABASE_URL is not set", errors.New("DATABASE_URL is required"))
	}

	if err := run(port, dsn); err != nil {
		o.Fatal("server error", err)
	}
	log.Info("stopped")
}

func run(port, dsn string) error {
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

	// gRPC server with OTel tracing + Prometheus metrics interceptors, plus a panic guard
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(obs.UnaryServerInterceptor(), grpcprom.UnaryServerInterceptor, recoverUnary),
		grpc.ChainStreamInterceptor(obs.StreamServerInterceptor(), grpcprom.StreamServerInterceptor),
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
	case <-time.After(drainTimeout):
		log.Warnf("graceful stop timed out after %s, closing remaining connections", drainTimeout)
		srv.Stop()
	}
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
		log.WithError(err).Warnf("database not ready (attempt %d), retrying in 2s", attempt)
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
			log.WithError(err).Warn("health: NOT_SERVING, database unreachable")
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
			log.WithContext(ctx).WithFields(logrus.Fields{
				"method": info.FullMethod,
				"panic":  fmt.Sprint(r),
				"stack":  string(debug.Stack()),
			}).Error("recovered from handler panic")
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}
