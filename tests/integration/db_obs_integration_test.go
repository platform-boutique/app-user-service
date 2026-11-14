//go:build integration

package integration

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"user-service/internal/obs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func obsTestPool(t *testing.T, dsn string, tp trace.TracerProvider) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	obs.InstrumentPool(cfg, tp)
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// A request that touches the database leaves CLIENT spans below the request span, named by the SQL operation, with
// db.system and without query parameters; the pool statistics move.
func TestDatabaseSpansAndPoolStats(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	p := obsTestPool(t, freshDB(t), tp)

	ctx, root := tp.Tracer("test").Start(context.Background(), "request")
	if _, err := p.Exec(ctx, "SELECT $1::text", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	root.End()

	var query sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "SELECT" && s.Parent().SpanID() == root.SpanContext().SpanID() {
			query = s
		}
	}
	if query == nil {
		t.Fatalf("no SELECT span below the request span; got %d spans", len(rec.Ended()))
	}
	if query.SpanKind() != trace.SpanKindClient {
		t.Errorf("kind = %v, want client", query.SpanKind())
	}
	attrs := map[attribute.Key]string{}
	for _, a := range query.Attributes() {
		attrs[a.Key] = a.Value.String()
		if a.Value.String() == "ada@example.com" {
			t.Errorf("attribute %s carries the query parameter", a.Key)
		}
	}
	if attrs["db.system.name"] != "postgresql" && attrs["db.system"] != "postgresql" {
		t.Errorf("no postgresql db.system attribute: %v", attrs)
	}

	reg := prometheus.NewRegistry()
	if err := obs.RegisterPoolMetrics(reg, p); err != nil {
		t.Fatal(err)
	}
	st := obs.PoolStatOf(p)
	if st.AcquireCount == 0 || st.NewConns == 0 || st.Max == 0 {
		t.Errorf("pool stat did not move: %+v", st)
	}
}

// The deadline interceptor ends a call that waits for the database: the handler's query is cancelled by the server's
// bound, the client sees DEADLINE_EXCEEDED and the query does not run for its full 60 s.
func TestRequestTimeoutEndsASlowDatabaseCall(t *testing.T) {
	p := obsTestPool(t, freshDB(t), nil)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(obs.DeadlineUnaryInterceptor(500 * time.Millisecond)))
	h := health.NewServer()
	healthpb.RegisterHealthServer(srv, &slowHealth{Server: h, pool: p})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second) // the client sets no tight deadline
	defer cancel()
	start := time.Now()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("code = %v (%v), want DeadlineExceeded", status.Code(err), err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("the call took %s: the server bound did not cut the 60 s query", time.Since(start))
	}
}

// slowHealth answers Check with a 60 s Postgres sleep run on the request context.
type slowHealth struct {
	*health.Server
	pool *pgxpool.Pool
}

func (s *slowHealth) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	_, err := s.pool.Exec(ctx, "SELECT pg_sleep(60)")
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	return nil, err
}
