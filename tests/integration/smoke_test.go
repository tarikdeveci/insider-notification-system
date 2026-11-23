package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func TestPostgresSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testutil.PostgresDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1: %v %d", err, one)
	}
}

func TestRedisSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opt, err := goredis.ParseURL(testutil.RedisURL(t))
	if err != nil {
		t.Fatal(err)
	}
	c := goredis.NewClient(opt)
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("close redis client: %v", err)
		}
	}()
	if got := c.Ping(ctx).Val(); got != "PONG" {
		t.Fatalf("ping = %q", got)
	}
}
