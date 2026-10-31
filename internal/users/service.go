package users

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	pb "user-service/proto"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Defaults.
const (
	DefaultBcryptCost = 11
	DefaultSessionTTL = 14 * 24 * time.Hour
	// MinBcryptCost / MaxBcryptCost bound BCRYPT_COST (bcrypt itself allows 4..31; above 16 a login takes minutes).
	MinBcryptCost = bcrypt.MinCost
	MaxBcryptCost = 16

	cleanupEvery   = time.Minute
	cleanupTimeout = 10 * time.Second
)

// ErrorMsgLogin is the one message of every failed login, whatever was wrong.
const ErrorMsgLogin = "wrong email or password"

// Config configures a Service.
type Config struct {
	BcryptCost      int           // 0 = DefaultBcryptCost
	SessionTTL      time.Duration // 0 = DefaultSessionTTL
	LoginLimiter    *RateLimiter  // per e-mail address; nil = none
	RegisterLimiter *RateLimiter  // one bucket for the process; nil = none
	Now             func() time.Time
}

// Service implements the gRPC UserService.
type Service struct {
	pb.UnimplementedUserServiceServer
	store    Store
	log      *logrus.Logger
	cost     int
	ttl      time.Duration
	login    *RateLimiter
	register *RateLimiter
	now      func() time.Time
	ready    atomic.Bool

	// dummyHash is compared against when the e-mail is unknown, so that a failed login costs one bcrypt
	// comparison either way.
	dummyHash []byte

	lastCleanup atomic.Int64 // unix nanos of the last clean-up that was started
	bg          sync.WaitGroup
}

// NewService creates the service. It hashes one random password at the configured cost (the dummy for
// unknown e-mails), so it takes about as long as one login. It answers UNAVAILABLE until SetReady(true).
func NewService(store Store, log *logrus.Logger, cfg Config) (*Service, error) {
	if cfg.BcryptCost == 0 {
		cfg.BcryptCost = DefaultBcryptCost
	}
	if cfg.BcryptCost < MinBcryptCost || cfg.BcryptCost > MaxBcryptCost {
		return nil, fmt.Errorf("bcrypt cost %d outside %d-%d", cfg.BcryptCost, MinBcryptCost, MaxBcryptCost)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	dummy, err := bcrypt.GenerateFromPassword(random, cfg.BcryptCost)
	if err != nil {
		return nil, err
	}
	return &Service{
		store: store, log: log, cost: cfg.BcryptCost, ttl: cfg.SessionTTL,
		login: cfg.LoginLimiter, register: cfg.RegisterLimiter, now: cfg.Now, dummyHash: dummy,
	}, nil
}

// SetReady switches the handlers on or off.
func (s *Service) SetReady(ok bool) { s.ready.Store(ok) }

// Wait blocks until background clean-ups that were started have finished (tests, shutdown).
func (s *Service) Wait() { s.bg.Wait() }

func (s *Service) checkReady() error {
	if !s.ready.Load() {
		return status.Error(codes.Unavailable, "database not ready")
	}
	return nil
}

// clock returns the current time in UTC, cut to Postgres' microsecond precision so that a stored value
// and the one returned to the caller are identical.
func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// toStatus maps internal errors to gRPC status codes without leaking SQL details to the caller.
func (s *Service) toStatus(method string, err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, ErrUnavailable):
		s.log.Warnf("[%s] database unavailable: %v", method, err)
		return status.Error(codes.Unavailable, "database unavailable")
	default:
		s.log.Errorf("[%s] error: %v", method, err)
		return status.Error(codes.Internal, "internal error")
	}
}

func tooManyAttempts(retry time.Duration) error {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return status.Errorf(codes.ResourceExhausted, "too many attempts, try again in %d seconds", secs)
}

func userPB(u User) *pb.User {
	return &pb.User{Id: u.ID, Email: u.Email, Name: u.Name, CreatedAt: timestamppb.New(u.CreatedAt)}
}

// newSession creates a token and the row to store for it.
func (s *Service) newSession(now time.Time) (token string, ns NewSession, err error) {
	token, err = NewToken()
	if err != nil {
		return "", NewSession{}, err
	}
	return token, NewSession{TokenHash: HashToken(token), CreatedAt: now, ExpiresAt: now.Add(s.ttl)}, nil
}

func (s *Service) authResponse(u User, token string, ns NewSession) *pb.AuthResponse {
	return &pb.AuthResponse{User: userPB(u), Token: token, ExpiresAt: timestamppb.New(ns.ExpiresAt)}
}

// Register creates the account and signs it in.
func (s *Service) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.AuthResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	email, password, name, err := ValidateRegister(req)
	if err != nil {
		return nil, s.toStatus("Register", err)
	}
	if ok, retry := s.register.Allow("register"); !ok {
		s.log.Warn("[Register] rate limit reached")
		return nil, tooManyAttempts(retry)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost)
	if err != nil {
		return nil, s.toStatus("Register", err)
	}
	now := s.clock()
	token, ns, err := s.newSession(now)
	if err != nil {
		return nil, s.toStatus("Register", err)
	}
	u := User{ID: uuid.NewString(), Email: email, Name: name, CreatedAt: now}
	// No "does it exist" pre-check: the unique index decides, so two concurrent registrations of one
	// address produce exactly one account.
	if err := s.store.CreateUser(ctx, Credentials{User: u, PasswordHash: string(hash)}, ns); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			return nil, status.Error(codes.AlreadyExists, "an account with this email already exists")
		}
		return nil, s.toStatus("Register", err)
	}
	s.log.Infof("registered user %s", u.ID)
	s.maybeCleanup(now)
	return s.authResponse(u, token, ns), nil
}

// Login checks the credentials and opens a session.
func (s *Service) Login(ctx context.Context, req *pb.LoginRequest) (*pb.AuthResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	email, password, err := ValidateLogin(req)
	if err != nil {
		return nil, s.toStatus("Login", err)
	}
	// The attempt is counted before the password is checked (see RateLimiter), for known and unknown
	// addresses alike.
	if ok, retry := s.login.Allow(email); !ok {
		s.log.Warn("[Login] rate limit reached for an address")
		return nil, tooManyAttempts(retry)
	}

	creds, err := s.store.FindByEmail(ctx, email)
	known := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, s.toStatus("Login", err)
	}
	hash := []byte(creds.PasswordHash)
	if !known {
		hash = s.dummyHash
	}
	// bcrypt ignores everything after 72 bytes: a longer password must never match a shorter one's hash.
	// It still costs one comparison (against the dummy) and fails like any other wrong password.
	matches := false
	if len(password) <= MaxPasswordLen {
		matches = bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	} else {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password[:MaxPasswordLen]))
	}
	if !known || !matches {
		s.log.Info("[Login] failed attempt")
		return nil, status.Error(codes.Unauthenticated, ErrorMsgLogin)
	}

	now := s.clock()
	token, ns, err := s.newSession(now)
	if err != nil {
		return nil, s.toStatus("Login", err)
	}
	if err := s.store.CreateSession(ctx, creds.ID, ns); err != nil {
		return nil, s.toStatus("Login", err)
	}
	s.login.Reset(email)
	s.log.Infof("login of user %s", creds.ID)
	s.maybeCleanup(now)
	return s.authResponse(creds.User, token, ns), nil
}

var errSession = status.Error(codes.Unauthenticated, "invalid or expired session")

// GetSession resolves a token to its user: unknown, expired and revoked tokens are all UNAUTHENTICATED.
func (s *Service) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.User, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	token := req.GetToken()
	if !wellFormedToken(token) {
		return nil, errSession
	}
	u, err := s.store.SessionUser(ctx, HashToken(token), s.clock())
	if errors.Is(err, ErrNotFound) {
		return nil, errSession
	}
	if err != nil {
		return nil, s.toStatus("GetSession", err)
	}
	return userPB(u), nil
}

// Logout revokes the token. Unknown, malformed and already revoked tokens are fine (idempotent).
func (s *Service) Logout(ctx context.Context, req *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	token := req.GetToken()
	if wellFormedToken(token) {
		if err := s.store.RevokeSession(ctx, HashToken(token), s.clock()); err != nil {
			return nil, s.toStatus("Logout", err)
		}
	}
	return &pb.LogoutResponse{}, nil
}

// maybeCleanup deletes expired sessions in the background, at most once a minute per process. There is no
// cron: the work rides on the sign-ins that create new sessions.
func (s *Service) maybeCleanup(now time.Time) {
	last := s.lastCleanup.Load()
	if now.UnixNano()-last < int64(cleanupEvery) || !s.lastCleanup.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		n, err := s.store.DeleteExpired(ctx, now)
		switch {
		case err != nil:
			s.log.Warnf("expired-session clean-up failed: %v", err)
		case n > 0:
			s.log.Infof("deleted %d expired session(s)", n)
		}
	}()
}
