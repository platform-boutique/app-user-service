package users

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies all embedded migrations. goose holds a Postgres session advisory lock while it runs,
// so replicas that start at the same time apply each migration exactly once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]*goose.MigrationResult, error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockTimeout(1, 120), // retry every second for up to two minutes
	)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, err
	}
	res, err := provider.Up(ctx)
	if err != nil {
		return res, classify(err)
	}
	return res, nil
}

// Setup is the start-up path (there is no seed data): the migrations; returns how many were applied.
func Setup(ctx context.Context, pool *pgxpool.Pool) (applied int, err error) {
	res, err := Migrate(ctx, pool)
	if err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}
	return len(res), nil
}
