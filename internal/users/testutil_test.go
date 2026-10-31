package users

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 123_456_789, time.UTC)}
}
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type memSession struct {
	userID    string
	expiresAt time.Time
	revokedAt *time.Time
}

// memStore is an in-memory Store with the same semantics as PGStore (unique lower(email), live sessions).
type memStore struct {
	mu       sync.Mutex
	users    map[string]Credentials // by id
	sessions map[string]memSession  // by string(token hash)
	err      error                  // returned by every call when set
}

func newMemStore() *memStore {
	return &memStore{users: map[string]Credentials{}, sessions: map[string]memSession{}}
}

func (m *memStore) byEmail(email string) (Credentials, bool) {
	for _, c := range m.users {
		if strings.EqualFold(c.Email, email) {
			return c, true
		}
	}
	return Credentials{}, false
}

func (m *memStore) CreateUser(_ context.Context, c Credentials, s NewSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if _, ok := m.byEmail(c.Email); ok {
		return ErrEmailTaken
	}
	m.users[c.ID] = c
	m.sessions[string(s.TokenHash)] = memSession{userID: c.ID, expiresAt: s.ExpiresAt}
	return nil
}

func (m *memStore) FindByEmail(_ context.Context, email string) (Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return Credentials{}, m.err
	}
	c, ok := m.byEmail(email)
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return c, nil
}

func (m *memStore) CreateSession(_ context.Context, userID string, s NewSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sessions[string(s.TokenHash)] = memSession{userID: userID, expiresAt: s.ExpiresAt}
	return nil
}

func (m *memStore) SessionUser(_ context.Context, h []byte, now time.Time) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return User{}, m.err
	}
	s, ok := m.sessions[string(h)]
	if !ok || s.revokedAt != nil || !s.expiresAt.After(now) {
		return User{}, ErrNotFound
	}
	return m.users[s.userID].User, nil
}

func (m *memStore) RevokeSession(_ context.Context, h []byte, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if s, ok := m.sessions[string(h)]; ok && s.revokedAt == nil {
		s.revokedAt = &now
		m.sessions[string(h)] = s
	}
	return nil
}

func (m *memStore) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	var n int64
	for k, s := range m.sessions {
		if !s.expiresAt.After(now) {
			delete(m.sessions, k)
			n++
		}
	}
	return n, nil
}

func (m *memStore) Ping(context.Context) error { return m.err }

func (m *memStore) sessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// testEnv is a service on a memStore with a fake clock and a captured log.
type testEnv struct {
	svc   *Service
	store *memStore
	clock *fakeClock
	logs  *bytes.Buffer
	login *RateLimiter
}

func newTestService(t *testing.T) *testEnv {
	t.Helper()
	clk := newClock()
	st := newMemStore()
	logs := &bytes.Buffer{}
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	l.Formatter = &logrus.JSONFormatter{}
	l.Out = logs
	login := NewRateLimiter(5, time.Minute, clk.Now)
	svc, err := NewService(st, l, Config{
		BcryptCost: 4, LoginLimiter: login, RegisterLimiter: NewRateLimiter(100, time.Minute, clk.Now), Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetReady(true)
	t.Cleanup(svc.Wait)
	return &testEnv{svc: svc, store: st, clock: clk, logs: logs, login: login}
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func testLogger(w io.Writer) *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	l.Formatter = &logrus.JSONFormatter{}
	l.Out = w
	return l
}

func (m *memStore) setErr(err error) { m.mu.Lock(); m.err = err; m.mu.Unlock() }
