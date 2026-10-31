package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User is an account as the API shows it.
type User struct {
	ID        string
	Email     string // lower-cased
	Name      string
	CreatedAt time.Time
}

// Credentials is a stored account including its bcrypt hash (never leaves this package's callers' logs).
type Credentials struct {
	User
	PasswordHash string
}

// NewSession is a session about to be stored: only the hash of the token is known to the database.
type NewSession struct {
	TokenHash []byte
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Errors of the storage layer.
var (
	// ErrEmailTaken: the unique index on lower(email) rejected the insert.
	ErrEmailTaken = errors.New("email already registered")
	// ErrNotFound: no such user, or no live (known, unexpired, not revoked) session.
	ErrNotFound = errors.New("not found")
	// ErrUnavailable marks storage errors caused by an unreachable database; the gRPC layer maps it to UNAVAILABLE.
	ErrUnavailable = errors.New("database unavailable")
)

// Store is what the gRPC handlers need from the storage layer.
type Store interface {
	// CreateUser stores the account and its first session in one transaction (ErrEmailTaken).
	CreateUser(ctx context.Context, c Credentials, s NewSession) error
	// FindByEmail looks an account up by its normalised address (ErrNotFound).
	FindByEmail(ctx context.Context, email string) (Credentials, error)
	// CreateSession stores a session for an existing user.
	CreateSession(ctx context.Context, userID string, s NewSession) error
	// SessionUser returns the owner of a session that exists, has not expired at now and is not revoked
	// (ErrNotFound otherwise).
	SessionUser(ctx context.Context, tokenHash []byte, now time.Time) (User, error)
	// RevokeSession marks the session revoked. Idempotent: an unknown or already revoked session is not an error.
	RevokeSession(ctx context.Context, tokenHash []byte, now time.Time) error
	// DeleteExpired removes sessions that expired at or before now and returns how many.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
	// Ping checks that the database answers.
	Ping(ctx context.Context) error
}

// PGStore is the Postgres implementation of Store.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps an existing pool (the caller owns and closes it).
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrEmailTaken) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// the server answered: a real SQL error, except for "server shutting down / too many connections" classes
		if strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57") || pgErr.Code == "53300" {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return err
	}
	// anything else (dial errors, closed connections, timeouts) means the database is not reachable
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

const insertSession = `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`

// CreateUser implements Store.
func (s *PGStore) CreateUser(ctx context.Context, c Credentials, ns NewSession) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx,
		`INSERT INTO users (id, email, name, password_hash, created_at) VALUES ($1, $2, $3, $4, $5)`,
		c.ID, c.Email, c.Name, c.PasswordHash, c.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_lower_key" {
			return ErrEmailTaken
		}
		return classify(err)
	}
	if _, err = tx.Exec(ctx, insertSession, ns.TokenHash, c.ID, ns.CreatedAt, ns.ExpiresAt); err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}

// FindByEmail implements Store.
func (s *PGStore) FindByEmail(ctx context.Context, email string) (Credentials, error) {
	var c Credentials
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, email, name, password_hash, created_at FROM users WHERE lower(email) = lower($1)`, email).
		Scan(&c.ID, &c.Email, &c.Name, &c.PasswordHash, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credentials{}, ErrNotFound
	}
	if err != nil {
		return Credentials{}, classify(err)
	}
	return c, nil
}

// CreateSession implements Store.
func (s *PGStore) CreateSession(ctx context.Context, userID string, ns NewSession) error {
	_, err := s.pool.Exec(ctx, insertSession, ns.TokenHash, userID, ns.CreatedAt, ns.ExpiresAt)
	return classify(err)
}

// SessionUser implements Store.
func (s *PGStore) SessionUser(ctx context.Context, tokenHash []byte, now time.Time) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT u.id::text, u.email, u.name, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2`, tokenHash, now).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, classify(err)
	}
	return u, nil
}

// RevokeSession implements Store.
func (s *PGStore) RevokeSession(ctx context.Context, tokenHash []byte, now time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash, now)
	return classify(err)
}

// DeleteExpired implements Store.
func (s *PGStore) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

// Ping implements Store.
func (s *PGStore) Ping(ctx context.Context) error {
	var one int
	return classify(s.pool.QueryRow(ctx, `SELECT 1`).Scan(&one))
}
