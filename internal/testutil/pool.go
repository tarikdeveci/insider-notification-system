package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

// PoolFor opens a pgx pool on dsn and closes it when the test finishes.
func PoolFor(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// MigratedPool returns a pool on a fresh database with all migrations applied.
func MigratedPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := PostgresDSN(t)
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return PoolFor(t, dsn)
}
