package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "user-service/proto"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ctx = context.Background()

func code(err error) codes.Code { return status.Code(err) }

func register(t *testing.T, e *testEnv, email, password string) *pb.AuthResponse {
	t.Helper()
	r, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: email, Password: password, Name: "Ada"})
	if err != nil {
		t.Fatalf("Register(%q): %v", email, err)
	}
	return r
}

func login(e *testEnv, email, password string) (*pb.AuthResponse, error) {
	return e.svc.Login(ctx, &pb.LoginRequest{Email: email, Password: password})
}

func TestLifecycle(t *testing.T) {
	e := newTestService(t)
	reg := register(t, e, "  Ada@Example.com ", "correct horse")
	if reg.User.Email != "ada@example.com" || reg.User.Name != "Ada" || reg.User.Id == "" || reg.User.CreatedAt == nil {
		t.Fatalf("register user: %+v", reg.User)
	}
	if len(reg.User.Id) != 36 || reg.User.Id[14] != '4' {
		t.Errorf("id %q is not a UUID v4", reg.User.Id)
	}
	if !wellFormedToken(reg.Token) {
		t.Errorf("token %q", reg.Token)
	}
	if got := reg.ExpiresAt.AsTime(); !got.Equal(e.clock.Now().UTC().Truncate(time.Microsecond).Add(DefaultSessionTTL)) {
		t.Errorf("expires_at %s, want 14 days after now", got)
	}
	// Register signs in: the token works
	if u, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: reg.Token}); err != nil || u.Id != reg.User.Id {
		t.Fatalf("GetSession after Register: %v, %v", u, err)
	}
	// Login (other case of the address) gives a different token for the same user
	lg, err := login(e, "ADA@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if lg.User.Id != reg.User.Id || lg.Token == reg.Token || !wellFormedToken(lg.Token) {
		t.Fatalf("login: %+v", lg)
	}
	u, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: lg.Token})
	if err != nil || u.Id != reg.User.Id || u.Email != "ada@example.com" || u.Name != "Ada" || !u.CreatedAt.AsTime().Equal(reg.User.CreatedAt.AsTime()) {
		t.Fatalf("GetSession: %v, %v", u, err)
	}
	// Logout kills that token only
	if _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: lg.Token}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: lg.Token}); code(err) != codes.Unauthenticated {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: reg.Token}); err != nil {
		t.Fatalf("other session was affected: %v", err)
	}
}

func TestOnlyTheHashIsStored(t *testing.T) {
	e := newTestService(t)
	reg := register(t, e, "ada@example.com", "correct horse")
	for k := range e.store.sessions {
		if k == reg.Token || len(k) != 32 || k != string(HashToken(reg.Token)) {
			t.Errorf("stored key is not the SHA-256 of the token")
		}
	}
	for _, c := range e.store.users {
		if c.PasswordHash == "correct horse" || bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte("correct horse")) != nil {
			t.Errorf("password not stored as a bcrypt hash: %q", c.PasswordHash)
		}
		if cost, _ := bcrypt.Cost([]byte(c.PasswordHash)); cost != 4 {
			t.Errorf("hash cost %d, want the configured 4", cost)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newTestService(t)
	for field, req := range map[string]*pb.RegisterRequest{
		"email":    {Email: "nope", Password: "correct horse", Name: "Ada"},
		"password": {Email: "ada@example.com", Password: "short", Name: "Ada"},
		"name":     {Email: "ada@example.com", Password: "correct horse", Name: ""},
	} {
		_, err := e.svc.Register(ctx, req)
		if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", field, err)
		}
	}
	if len(e.store.users) != 0 {
		t.Error("an invalid request created an account")
	}
}

func TestRegisterDuplicate(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	for _, email := range []string{"ada@example.com", "ADA@EXAMPLE.COM", "  Ada@Example.Com  "} {
		_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: email, Password: "another pass", Name: "Eve"})
		if code(err) != codes.AlreadyExists {
			t.Errorf("%q: %v", email, err)
		}
	}
	if len(e.store.users) != 1 {
		t.Errorf("%d accounts", len(e.store.users))
	}
}

func TestRegisterConcurrentSameEmail(t *testing.T) {
	e := newTestService(t)
	var wg sync.WaitGroup
	var ok, exists atomic.Int32
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			email := "race@example.com"
			if i%2 == 1 {
				email = "RACE@Example.com"
			}
			_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: email, Password: "correct horse", Name: "Ada"})
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
	wg.Wait()
	if ok.Load() != 1 || exists.Load() != 19 {
		t.Errorf("%d successes, %d already exists; want 1 and 19", ok.Load(), exists.Load())
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	e.login = nil
	var compares atomic.Int32
	real := e.svc.compare
	e.svc.compare = func(h, p []byte) error { compares.Add(1); return real(h, p) }

	attempts := map[string][2]string{
		"wrong password":                {"ada@example.com", "wrong horse!!"},
		"unknown email":                 {"nobody@example.com", "correct horse"},
		"malformed email":               {"not an address", "correct horse"},
		"too long password":             {"ada@example.com", strings.Repeat("x", 100)},
		"correct start, too long":       {"ada@example.com", "correct horse" + strings.Repeat("\x00", 80)},
		"overlong email":                {strings.Repeat("a", 400) + "@example.com", "correct horse"},
		"password case":                 {"ada@example.com", "Correct Horse"},
		"short unknown":                 {"x@y.zz", "p"},
		"unknown email, 72-byte pass":   {"nobody@example.com", strings.Repeat("p", 72)},
		"known email, 72-byte password": {"ada@example.com", strings.Repeat("p", 72)},
	}
	var wantMsg string
	for name, a := range attempts {
		before := compares.Load()
		_, err := login(e, a[0], a[1])
		if code(err) != codes.Unauthenticated || status.Convert(err).Message() != ErrorMsgLogin {
			t.Errorf("%s: %v", name, err)
		}
		if wantMsg == "" {
			wantMsg = err.Error()
		} else if err.Error() != wantMsg {
			t.Errorf("%s: error text differs: %q vs %q", name, err.Error(), wantMsg)
		}
		if got := compares.Load() - before; got != 1 {
			t.Errorf("%s: %d bcrypt comparisons, want exactly 1 (same cost on every failure path)", name, got)
		}
	}
	if len(e.store.sessions) != 1 {
		t.Errorf("a failed login created a session (%d sessions)", len(e.store.sessions))
	}
}

func TestLoginPasswordLongerThan72BytesNeverMatchesItsPrefix(t *testing.T) {
	e := newTestService(t)
	prefix := strings.Repeat("a", 72)
	register(t, e, "ada@example.com", prefix)
	if _, err := login(e, "ada@example.com", prefix); err != nil {
		t.Fatalf("72-byte password: %v", err)
	}
	if _, err := login(e, "ada@example.com", prefix+"b"); code(err) != codes.Unauthenticated {
		t.Fatalf("73 bytes with the same first 72 bytes logged in: %v", err)
	}
}

func TestLoginMissingFields(t *testing.T) {
	e := newTestService(t)
	for _, r := range []*pb.LoginRequest{{}, {Email: "ada@example.com"}, {Password: "x"}} {
		if _, err := e.svc.Login(ctx, r); code(err) != codes.InvalidArgument {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	register(t, e, "bob@example.com", "correct horse")
	for i := range 5 {
		if _, err := login(e, "ada@example.com", "wrong horse!!"); code(err) != codes.Unauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// the 6th is refused, even with the right password; same for unknown addresses
	_, err := login(e, "ada@example.com", "correct horse")
	if code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "try again in") {
		t.Fatalf("6th attempt: %v", err)
	}
	for range 5 {
		_, _ = login(e, "ghost@example.com", "whatever pass")
	}
	if _, err := login(e, "ghost@example.com", "whatever pass"); code(err) != codes.ResourceExhausted {
		t.Fatalf("unknown address not limited the same way: %v", err)
	}
	// the case of the address does not give a new bucket
	if _, err := login(e, "ADA@example.com", "correct horse"); code(err) != codes.ResourceExhausted {
		t.Fatalf("other case of the address: %v", err)
	}
	// other addresses are not affected
	if _, err := login(e, "bob@example.com", "correct horse"); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// someone else's failures block for one window at most
	e.clock.Advance(61 * time.Second)
	if _, err := login(e, "ada@example.com", "correct horse"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestLoginSuccessResetsTheCount(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	for round := range 4 {
		for range 4 {
			_, _ = login(e, "ada@example.com", "wrong horse!!")
		}
		if _, err := login(e, "ada@example.com", "correct horse"); err != nil {
			t.Fatalf("round %d: success refused after 4 failures: %v", round, err)
		}
	}
}

func TestLoginParallelGuessesAreCountedBeforeBcrypt(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	var wg sync.WaitGroup
	var unauth, limited atomic.Int32
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := login(e, "ada@example.com", "wrong horse!!")
			switch code(err) {
			case codes.Unauthenticated:
				unauth.Add(1)
			case codes.ResourceExhausted:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if unauth.Load() != 5 || limited.Load() != 35 {
		t.Errorf("%d guesses got through, %d refused; want 5 and 35", unauth.Load(), limited.Load())
	}
}

func TestRegisterRateLimit(t *testing.T) {
	e := newTestService(t)
	e.svc.register = NewRateLimiter(2, time.Minute, e.clock.Now)
	register(t, e, "a1@example.com", "correct horse")
	register(t, e, "a2@example.com", "correct horse")
	_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: "a3@example.com", Password: "correct horse", Name: "A"})
	if code(err) != codes.ResourceExhausted {
		t.Fatalf("%v", err)
	}
	// invalid requests are refused before they count
	e.clock.Advance(2 * time.Minute)
	for range 10 {
		_, _ = e.svc.Register(ctx, &pb.RegisterRequest{Email: "bad", Password: "correct horse", Name: "A"})
	}
	register(t, e, "a3@example.com", "correct horse")
}

func TestSessionExpiry(t *testing.T) {
	e := newTestService(t)
	reg := register(t, e, "ada@example.com", "correct horse")
	get := func() error {
		_, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: reg.Token})
		return err
	}
	e.clock.Advance(DefaultSessionTTL - time.Second)
	if err := get(); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	e.clock.Advance(time.Second) // exactly expires_at: expired
	if err := get(); code(err) != codes.Unauthenticated {
		t.Fatalf("at expiry: %v", err)
	}
}

func TestSessionTTLConfigurable(t *testing.T) {
	st := newMemStore()
	clk := newClock()
	svc, err := NewService(st, testLogger(new(strings.Builder)), Config{BcryptCost: 4, SessionTTL: time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetReady(true)
	reg, err := svc.Register(ctx, &pb.RegisterRequest{Email: "ada@example.com", Password: "correct horse", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.ExpiresAt.AsTime().Sub(clk.Now()); got < 59*time.Minute+59*time.Second || got > time.Hour {
		t.Errorf("ttl %s", got)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	e := newTestService(t)
	reg := register(t, e, "ada@example.com", "correct horse")
	unknown, _ := NewToken()
	for _, tok := range []string{reg.Token, reg.Token, unknown, "", "garbage", strings.Repeat("A", 300)} {
		if _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: tok}); err != nil {
			t.Errorf("Logout(%q): %v", tok, err)
		}
	}
	if _, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: reg.Token}); code(err) != codes.Unauthenticated {
		t.Errorf("revoked token still valid: %v", err)
	}
}

func TestGetSessionRejectsEverythingUnknown(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	unknown, _ := NewToken()
	for _, tok := range []string{"", unknown, "garbage", strings.Repeat("A", 43)} {
		_, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: tok})
		if code(err) != codes.Unauthenticated || status.Convert(err).Message() != "invalid or expired session" {
			t.Errorf("GetSession(%q): %v", tok, err)
		}
	}
}

func TestRevokedSessionDoesNotComeBackOnLogin(t *testing.T) {
	e := newTestService(t)
	reg := register(t, e, "ada@example.com", "correct horse")
	_, _ = e.svc.Logout(ctx, &pb.LogoutRequest{Token: reg.Token})
	if _, err := login(e, "ada@example.com", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: reg.Token}); code(err) != codes.Unauthenticated {
		t.Errorf("revoked token valid again after a new login: %v", err)
	}
}

func TestExpiredSessionsAreDeletedOpportunistically(t *testing.T) {
	e := newTestService(t)
	register(t, e, "a@example.com", "correct horse") // starts a clean-up (finds nothing)
	e.svc.Wait()
	for range 3 {
		if _, err := login(e, "a@example.com", "correct horse"); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.store.sessionCount(); n != 4 {
		t.Fatalf("%d sessions", n)
	}
	e.clock.Advance(DefaultSessionTTL + time.Hour)
	if _, err := login(e, "a@example.com", "correct horse"); err != nil { // creates the 5th, triggers the clean-up
		t.Fatal(err)
	}
	e.svc.Wait()
	if n := e.store.sessionCount(); n != 1 {
		t.Errorf("%d sessions left, want only the new one", n)
	}
}

func TestCleanupIsThrottled(t *testing.T) {
	e := newTestService(t)
	var calls atomic.Int32
	cs := &countingStore{memStore: e.store, deletes: &calls}
	e.svc.store = cs
	register(t, e, "a@example.com", "correct horse")
	for range 5 {
		_, _ = login(e, "a@example.com", "correct horse")
	}
	e.svc.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d clean-ups within one minute, want 1", calls.Load())
	}
	e.clock.Advance(cleanupEvery)
	_, _ = login(e, "a@example.com", "correct horse")
	e.svc.Wait()
	if calls.Load() != 2 {
		t.Errorf("%d clean-ups after a minute, want 2", calls.Load())
	}
}

type countingStore struct {
	*memStore
	deletes *atomic.Int32
}

func (c *countingStore) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	c.deletes.Add(1)
	return c.memStore.DeleteExpired(ctx, now)
}

func TestNotReadyAndStorageErrors(t *testing.T) {
	e := newTestService(t)
	register(t, e, "ada@example.com", "correct horse")
	e.svc.SetReady(false)
	for name, f := range map[string]func() error{
		"Register":   func() error { _, err := e.svc.Register(ctx, &pb.RegisterRequest{}); return err },
		"Login":      func() error { _, err := e.svc.Login(ctx, &pb.LoginRequest{}); return err },
		"GetSession": func() error { _, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{}); return err },
		"Logout":     func() error { _, err := e.svc.Logout(ctx, &pb.LogoutRequest{}); return err },
	} {
		if code(f()) != codes.Unavailable {
			t.Errorf("%s while not ready: %v", name, f())
		}
	}
	e.svc.SetReady(true)

	tok, _ := NewToken()
	cases := []struct {
		err  error
		want codes.Code
	}{
		{fmt.Errorf("%w: connection refused", ErrUnavailable), codes.Unavailable},
		{errors.New("pq: syntax error at or near SELECT"), codes.Internal},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
	}
	for _, c := range cases {
		e.store.setErr(c.err)
		for name, f := range map[string]func() error{
			"Register": func() error {
				_, err := e.svc.Register(ctx, &pb.RegisterRequest{Email: "x@example.com", Password: "correct horse", Name: "X"})
				return err
			},
			"Login":      func() error { _, err := login(e, "ada@example.com", "correct horse"); return err },
			"GetSession": func() error { _, err := e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: tok}); return err },
			"Logout":     func() error { _, err := e.svc.Logout(ctx, &pb.LogoutRequest{Token: tok}); return err },
		} {
			err := f()
			if code(err) != c.want {
				t.Errorf("%s with %v: %v, want %v", name, c.err, err, c.want)
			}
			if strings.Contains(err.Error(), "SELECT") || strings.Contains(err.Error(), "refused") {
				t.Errorf("%s leaks the storage error: %v", name, err)
			}
		}
	}
}

func TestNewServiceRejectsBadCost(t *testing.T) {
	for _, c := range []int{-1, 3, 17, 31} {
		if _, err := NewService(newMemStore(), testLogger(new(strings.Builder)), Config{BcryptCost: c}); err == nil {
			t.Errorf("cost %d accepted", c)
		}
	}
	if s, err := NewService(newMemStore(), testLogger(new(strings.Builder)), Config{}); err != nil {
		t.Errorf("default config: %v", err)
	} else if cost, _ := bcrypt.Cost(s.dummyHash); cost != DefaultBcryptCost {
		t.Errorf("default cost %d", cost)
	}
}

// TestNothingSecretIsLogged runs every RPC, with successes and failures, at debug level and checks that no
// log line contains a password, a token, a token hash or a password hash.
func TestNothingSecretIsLogged(t *testing.T) {
	e := newTestService(t)
	const pw = "S3cret-p4ssw0rd-MARKER"
	const wrong = "Wr0ng-p4ssw0rd-MARKER"
	reg := register(t, e, "marker.user@example.com", pw)
	lg, err := login(e, "marker.user@example.com", pw)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = login(e, "marker.user@example.com", wrong)
	_, _ = login(e, "ghost@example.com", wrong)
	_, _ = e.svc.Register(ctx, &pb.RegisterRequest{Email: "marker.user@example.com", Password: wrong, Name: "Dup"})
	_, _ = e.svc.Register(ctx, &pb.RegisterRequest{Email: "bad", Password: wrong, Name: "Bad"})
	for range 6 { // reach the rate limit
		_, _ = login(e, "marker.user@example.com", wrong)
	}
	_, _ = e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: lg.Token})
	_, _ = e.svc.Logout(ctx, &pb.LogoutRequest{Token: lg.Token})
	_, _ = e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: lg.Token})
	e.store.setErr(fmt.Errorf("%w: connection refused", ErrUnavailable))
	_, _ = e.svc.GetSession(ctx, &pb.GetSessionRequest{Token: reg.Token})
	_, _ = login(e, "marker.user@example.com", wrong)
	e.svc.Wait()

	out := e.logs.String()
	if !strings.Contains(out, "registered user") || !strings.Contains(out, "login of user") {
		t.Fatalf("the test sees no log lines, it proves nothing:\n%s", out)
	}
	var secrets []string
	secrets = append(secrets, pw, wrong, reg.Token, lg.Token)
	for _, tok := range []string{reg.Token, lg.Token} {
		h := HashToken(tok)
		secrets = append(secrets, fmt.Sprintf("%x", h), fmt.Sprintf("%X", h), string(h), fmt.Sprintf("%v", h))
		secrets = append(secrets, strings.TrimRight(base64Std(h), "="), base64Std(h))
	}
	for _, c := range e.store.users {
		secrets = append(secrets, c.PasswordHash, c.PasswordHash[7:])
	}
	secrets = append(secrets, string(e.svc.dummyHash))
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("a log line contains %q", s)
		}
	}
	if strings.Contains(out, "marker.user@example.com") {
		t.Errorf("an e-mail address is logged:\n%s", out)
	}
}
