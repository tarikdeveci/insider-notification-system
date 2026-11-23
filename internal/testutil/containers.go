// Package testutil provides shared Postgres and Redis containers for tests.
//
// One container of each kind is started lazily per test process. They are not
// terminated explicitly: the testcontainers Ryuk reaper removes them when the
// process exits.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

var (
	pgOnce sync.Once
	pgBase string
	pgErr  error

	redisOnce sync.Once
	redisURL  string
	redisErr  error
)

func startPostgres() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		pgErr = err
		return
	}
	pgBase, pgErr = c.ConnectionString(ctx, "sslmode=disable")
}

func startRedis() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := redis.Run(ctx, "redis:7-alpine")
	if err != nil {
		redisErr = err
		return
	}
	redisURL, redisErr = c.ConnectionString(ctx)
}

// PostgresDSN returns the DSN of a fresh, empty database inside the shared
// Postgres container. The database is dropped when the test finishes.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	pgOnce.Do(startPostgres)
	if pgErr != nil {
		t.Fatalf("start postgres container: %v", pgErr)
	}

	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := "t_" + hex.EncodeToString(b[:])
	quoted := pgx.Identifier{name}.Sanitize()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, pgBase)
	if err != nil {
		t.Fatalf("connect to postgres container: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, pgBase)
		if err != nil {
			t.Errorf("drop database: connect: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})

	u, err := url.Parse(pgBase)
	if err != nil {
		t.Fatalf("parse postgres url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// RedisURL returns the URL of the shared Redis container.
func RedisURL(t testing.TB) string {
	t.Helper()
	redisOnce.Do(startRedis)
	if redisErr != nil {
		t.Fatalf("start redis container: %v", redisErr)
	}
	return redisURL
}
