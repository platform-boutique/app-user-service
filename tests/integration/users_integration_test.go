//go:build integration

// Integration tests against a real Postgres. Run with:
//
//	TEST_DATABASE_URL=postgres://users:users@127.0.0.1:55436/postgres?sslmode=disable \
//	  go test -race -tags integration ./tests/integration/
//
// Every test creates (and drops) its own database, so they need a user that may CREATE DATABASE.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"user-service/internal/users"
	pb "user-service/proto"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var ctx = context.Background()

// freshDB creates an empty database and returns its DSN.
func freshDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	name := fmt.Sprintf("users_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func newPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	svc   *users.Service
	pool  *pgxpool.Pool
	clock *clock
	logs  *bytes.Buffer
}

// setup migrates a fresh database and returns a service on it (bcrypt cost 4, login limit 5 per minute).
func setup(t *testing.T) *env {
	t.Helper()
	p := newPool(t, freshDB(t))
	if _, err := users.Setup(ctx, p); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	l.Out = logs
	clk := &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 123_456_789, time.UTC)} // sub-microsecond part on purpose
	svc, err := users.NewService(users.NewPGStore(p), l, users.Config{
		BcryptCost:      4,
		LoginLimiter:    users.NewRateLimiter(5, time.Minute, clk.Now),
		RegisterLimiter: users.NewRateLimiter(1000, time.Minute, clk.Now),
		Now:             clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetReady(true)
	t.Cleanup(svc.Wait)
	return &env{svc: svc, pool: p, clock: clk, logs: logs}
}

func code(err error) codes.Code { return status.Code(err) }

func (e *env) register(t *testing.T, email, password string) *pb.AuthResponse {
	t.Helper()
	r, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: email, Password: password, Name: "Ada"})
	if err != nil {
		t.Fatalf("Register(%q): %v", email, err)
	}
	return r
}

func (e *env) login(email, password string) (*pb.AuthResponse, error) {
	return e.svc.Login(ctx, &pb.LoginRequest{Email: email, Password: password})
}

func (e *env) session(token string) (*pb.User, error) {
	return e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: token})
}

func count(t *testing.T, p *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrationsAreIdempotentAndConcurrentStartsApplyOnce(t *testing.T) {
	p := newPool(t, freshDB(t))
	var wg sync.WaitGroup
	var total atomic.Int32
	for range 6 { // six replicas starting together
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := users.Setup(ctx, p)
			if err != nil {
				t.Errorf("Setup: %v", err)
			}
			total.Add(int32(n))
		}()
	}
	wg.Wait()
	if total.Load() != 1 {
		t.Errorf("%d migrations applied in total, want exactly 1", total.Load())
	}
	if n, err := users.Setup(ctx, p); err != nil || n != 0 {
		t.Errorf("second Setup: %d, %v", n, err)
	}
	if n := count(t, p, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('users','sessions')`); n != 2 {
		t.Errorf("%d tables", n)
	}
}

func TestRegisterLoginSessionLogout(t *testing.T) {
	e := setup(t)
	reg := e.register(t, " Ada@Example.COM ", "correct horse")
	if reg.User.Email != "ada@example.com" || reg.User.Name != "Ada" || len(reg.User.Id) != 36 {
		t.Fatalf("register: %+v", reg.User)
	}
	// register signed in
	if u, err := e.session(reg.Token); err != nil || u.Id != reg.User.Id {
		t.Fatalf("GetSession with the register token: %v, %v", u, err)
	}
	lg, err := e.login("ada@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if lg.User.Id != reg.User.Id || lg.Token == reg.Token {
		t.Fatalf("login: %+v", lg)
	}
	u, err := e.session(lg.Token)
	if err != nil {
		t.Fatal(err)
	}
	// what the caller got and what was stored are identical (microsecond precision)
	if u.Id != reg.User.Id || u.Email != "ada@example.com" || !u.CreatedAt.AsTime().Equal(reg.User.CreatedAt.AsTime()) {
		t.Fatalf("GetSession: %v vs %v", u, reg.User)
	}
	if !lg.ExpiresAt.AsTime().Equal(e.clock.Now().Truncate(time.Microsecond).Add(14 * 24 * time.Hour)) {
		t.Errorf("expires_at %s", lg.ExpiresAt.AsTime())
	}
	if _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: lg.Token}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.session(lg.Token); code(err) != codes.Unauthenticated {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := e.session(reg.Token); err != nil {
		t.Fatalf("the other session was revoked too: %v", err)
	}
	// Logout twice, with an unknown and a malformed token: all fine
	unknown, _ := users.NewToken()
	for _, tok := range []string{lg.Token, unknown, "garbage", ""} {
		if _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: tok}); err != nil {
			t.Errorf("Logout(%q): %v", tok, err)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM sessions WHERE revoked_at IS NOT NULL`); n != 1 {
		t.Errorf("%d revoked sessions, want 1", n)
	}
}

func TestOnlyHashesAreStored(t *testing.T) {
	e := setup(t)
	reg := e.register(t, "ada@example.com", "correct horse")
	lg, _ := e.login("ada@example.com", "correct horse")
	for _, tok := range []string{reg.Token, lg.Token} {
		h := sha256.Sum256([]byte(tok))
		if n := count(t, e.pool, `SELECT count(*) FROM sessions WHERE token_hash = $1`, h[:]); n != 1 {
			t.Errorf("no session row with the SHA-256 of the token")
		}
	}
	// the token text and the password appear in no column of any row
	for _, table := range []string{"users", "sessions"} {
		var dump string
		if err := e.pool.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM %s t`, table)).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{reg.Token, lg.Token, "correct horse"} {
			if strings.Contains(dump, secret) {
				t.Errorf("table %s contains %q", table, secret)
			}
		}
	}
	var hash string
	if err := e.pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != 4 {
		t.Errorf("password_hash %q: cost %d, %v", hash, cost, err)
	}
}

func TestDuplicateEmailIncludingOtherCase(t *testing.T) {
	e := setup(t)
	e.register(t, "ada@example.com", "correct horse")
	for _, email := range []string{"ada@example.com", "ADA@example.com", " Ada@Example.Com ", "ADA@EXAMPLE.COM"} {
		_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: email, Password: "other password", Name: "Eve"})
		if code(err) != codes.AlreadyExists {
			t.Errorf("%q: %v", email, err)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("%d users", n)
	}
	// the constraint itself holds, not only the service: a direct insert in other case fails
	_, err := e.pool.Exec(ctx, `INSERT INTO users (id, email, name, password_hash, created_at)
		VALUES (gen_random_uuid(), 'ADA@Example.com', 'X', 'x', now())`)
	if err == nil || !strings.Contains(err.Error(), "users_email_lower_key") {
		t.Errorf("direct insert: %v", err)
	}
	// the first account is untouched: its password still works, the second one's does not
	if _, err := e.login("ada@example.com", "correct horse"); err != nil {
		t.Errorf("original password: %v", err)
	}
	if _, err := e.login("ada@example.com", "other password"); code(err) != codes.Unauthenticated {
		t.Errorf("password of the refused registration: %v", err)
	}
}

func TestConcurrentDuplicateRegistersGiveExactlyOneAccount(t *testing.T) {
	e := setup(t)
	for round := range 3 {
		email := fmt.Sprintf("race%d@example.com", round)
		var wg sync.WaitGroup
		var ok, exists atomic.Int32
		start := make(chan struct{})
		for i := range 30 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				addr := email
				if i%3 == 1 {
					addr = strings.ToUpper(email)
				} else if i%3 == 2 {
					addr = "  " + email + " "
				}
				<-start
				_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: addr, Password: "correct horse", Name: "Ada"})
				switch code(err) {
				case codes.OK:
					ok.Add(1)
				case codes.AlreadyExists:
					exists.Add(1)
				default:
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if ok.Load() != 1 || exists.Load() != 29 {
			t.Errorf("round %d: %d successes, %d already exists; want 1 and 29", round, ok.Load(), exists.Load())
		}
		if n := count(t, e.pool, `SELECT count(*) FROM users WHERE lower(email) = $1`, email); n != 1 {
			t.Errorf("round %d: %d accounts", round, n)
		}
		// the losers left no session behind: one session for the one account
		if n := count(t, e.pool, `SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE lower(u.email) = $1`, email); n != 1 {
			t.Errorf("round %d: %d sessions for the account", round, n)
		}
	}
}

func TestWrongPasswordAndUnknownEmailAreIdentical(t *testing.T) {
	e := setup(t)
	e.register(t, "ada@example.com", "correct horse")
	_, errWrong := e.login("ada@example.com", "wrong horse!!")
	_, errUnknown := e.login("nobody@example.com", "wrong horse!!")
	if errWrong == nil || errUnknown == nil {
		t.Fatal("login succeeded")
	}
	if code(errWrong) != codes.Unauthenticated || code(errWrong) != code(errUnknown) {
		t.Errorf("codes: %v vs %v", code(errWrong), code(errUnknown))
	}
	if status.Convert(errWrong).Message() != "wrong email or password" || errWrong.Error() != errUnknown.Error() {
		t.Errorf("messages: %q vs %q", errWrong, errUnknown)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("%d sessions, a failed login created one", n)
	}
}

func TestExpiredSession(t *testing.T) {
	e := setup(t)
	reg := e.register(t, "ada@example.com", "correct horse")
	e.clock.Advance(14*24*time.Hour - time.Second)
	if _, err := e.session(reg.Token); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	e.clock.Advance(time.Second)
	if _, err := e.session(reg.Token); code(err) != codes.Unauthenticated {
		t.Fatalf("at expiry: %v", err)
	}
	// logging in again works and gives a live session
	lg, err := e.login("ada@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.session(lg.Token); err != nil {
		t.Fatal(err)
	}
}

func TestRevokedSession(t *testing.T) {
	e := setup(t)
	reg := e.register(t, "ada@example.com", "correct horse")
	if _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: reg.Token}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.session(reg.Token); code(err) != codes.Unauthenticated {
		t.Fatalf("revoked: %v", err)
	}
	// the revocation time is the first one: a second Logout does not move it
	var first time.Time
	h := sha256.Sum256([]byte(reg.Token))
	if err := e.pool.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE token_hash = $1`, h[:]).Scan(&first); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	_, _ = e.svc.Logout(ctx, &pb.LogoutRequest{Token: reg.Token})
	var second time.Time
	_ = e.pool.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE token_hash = $1`, h[:]).Scan(&second)
	if !first.Equal(second) {
		t.Errorf("revoked_at moved from %s to %s", first, second)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := setup(t)
	e.register(t, "ada@example.com", "correct horse")
	e.register(t, "bob@example.com", "correct horse")
	for i := range 5 {
		if _, err := e.login("ada@example.com", "wrong horse!!"); code(err) != codes.Unauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := e.login("ada@example.com", "correct horse")
	if code(err) != codes.ResourceExhausted {
		t.Fatalf("6th attempt: %v", err)
	}
	if _, err := e.login("bob@example.com", "correct horse"); err != nil {
		t.Errorf("another address is affected: %v", err)
	}
	e.clock.Advance(61 * time.Second)
	if _, err := e.login("ada@example.com", "correct horse"); err != nil {
		t.Errorf("still blocked after the window: %v", err)
	}
}

func TestExpiredSessionsAreDeletedByLogin(t *testing.T) {
	e := setup(t)
	e.register(t, "ada@example.com", "correct horse")
	for range 3 {
		if _, err := e.login("ada@example.com", "correct horse"); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.Wait()
	if n := count(t, e.pool, `SELECT count(*) FROM sessions`); n != 4 {
		t.Fatalf("%d sessions", n)
	}
	e.clock.Advance(15 * 24 * time.Hour)
	if _, err := e.login("ada@example.com", "correct horse"); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if n := count(t, e.pool, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("%d sessions left, want only the new one", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("the user was deleted with its sessions")
	}
}

func TestConstraints(t *testing.T) {
	e := setup(t)
	bad := []string{
		`INSERT INTO users VALUES (gen_random_uuid(), 'ab', 'X', 'h', now())`,                                                     // email too short
		`INSERT INTO users VALUES (gen_random_uuid(), 'a@b.cd', '', 'h', now())`,                                                  // empty name
		`INSERT INTO users VALUES (gen_random_uuid(), 'a@b.cd', repeat('x', 101), 'h', now())`,                                    // long name
		`INSERT INTO sessions VALUES ('\x00', gen_random_uuid(), now(), now() + interval '1 day', NULL)`,                          // short hash
		`INSERT INTO sessions VALUES (decode(repeat('00', 32), 'hex'), gen_random_uuid(), now(), now() + interval '1 day', NULL)`, // no such user
	}
	for _, q := range bad {
		if _, err := e.pool.Exec(ctx, q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func TestDatabaseDownIsUnavailableWithoutDetails(t *testing.T) {
	e := setup(t)
	reg := e.register(t, "ada@example.com", "correct horse")
	e.pool.Close()
	for name, f := range map[string]func() error{
		"GetSession": func() error { _, err := e.session(reg.Token); return err },
		"Login":      func() error { _, err := e.login("ada@example.com", "correct horse"); return err },
		"Logout":     func() error { _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: reg.Token}); return err },
		"Register": func() error {
			_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: "x@example.com", Password: "correct horse", Name: "X"})
			return err
		},
	} {
		err := f()
		if code(err) != codes.Unavailable || status.Convert(err).Message() != "database unavailable" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestOverGRPC runs the whole flow over a real gRPC connection (what the frontend sees).
func TestOverGRPC(t *testing.T) {
	e := setup(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterUserServiceServer(srv, e.svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pb.NewUserServiceClient(conn)

	reg, err := c.Register(ctx, &pb.RegisterRequest{Email: "Grace@Example.com", Password: "hopper-1906", Name: " Grace "})
	if err != nil || reg.User.Name != "Grace" || reg.User.Email != "grace@example.com" || len(reg.Token) != 43 || reg.ExpiresAt == nil {
		t.Fatalf("Register: %v, %v", reg, err)
	}
	if _, err := c.Register(ctx, &pb.RegisterRequest{Email: "grace@example.com", Password: "hopper-1906", Name: "G"}); code(err) != codes.AlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := c.Register(ctx, &pb.RegisterRequest{Email: "x@example.com", Password: "short", Name: "G"}); code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "password") {
		t.Errorf("short password: %v", err)
	}
	if _, err := c.Login(ctx, &pb.LoginRequest{Email: "grace@example.com", Password: "nope-nope-nope"}); code(err) != codes.Unauthenticated {
		t.Errorf("wrong password: %v", err)
	}
	lg, err := c.Login(ctx, &pb.LoginRequest{Email: "grace@example.com", Password: "hopper-1906"})
	if err != nil {
		t.Fatal(err)
	}
	if u, err := c.GetSession(ctx, &pb.GetSessionRequest{Token: lg.Token}); err != nil || u.Id != reg.User.Id {
		t.Fatalf("GetSession: %v, %v", u, err)
	}
	if _, err := c.Logout(ctx, &pb.LogoutRequest{Token: lg.Token}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSession(ctx, &pb.GetSessionRequest{Token: lg.Token}); code(err) != codes.Unauthenticated {
		t.Errorf("after logout: %v", err)
	}
}

func TestNoSecretsInLogs(t *testing.T) {
	e := setup(t)
	reg := e.register(t, "ada@example.com", "S3cret-MARKER-pw")
	lg, _ := e.login("ada@example.com", "S3cret-MARKER-pw")
	_, _ = e.login("ada@example.com", "Wr0ng-MARKER-pw")
	_, _ = e.session(lg.Token)
	_, _ = e.svc.Logout(ctx, &pb.LogoutRequest{Token: lg.Token})
	e.pool.Close()
	_, _ = e.session(reg.Token) // an error path with a real driver error
	_, _ = e.login("ada@example.com", "S3cret-MARKER-pw")
	e.svc.Wait()
	out := e.logs.String()
	if out == "" {
		t.Fatal("no log output, the test proves nothing")
	}
	for _, secret := range []string{"S3cret-MARKER-pw", "Wr0ng-MARKER-pw", reg.Token, lg.Token} {
		if strings.Contains(out, secret) {
			t.Errorf("a log line contains %q", secret)
		}
	}
	for _, tok := range []string{reg.Token, lg.Token} {
		if h := sha256.Sum256([]byte(tok)); strings.Contains(out, fmt.Sprintf("%x", h)) {
			t.Errorf("a log line contains the hash of a token")
		}
	}
	if strings.Contains(out, "$2a$") || strings.Contains(out, "$2b$") {
		t.Errorf("a log line contains a bcrypt hash")
	}
}
