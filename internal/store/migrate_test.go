package store_test

import (
	"context"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	dsn := testutil.PostgresDSN(t)
	if err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("second run must be a no-op: %v", err)
	}
	pool := testutil.PoolFor(t, dsn)
	ctx := context.Background()

	cols := map[string][]string{
		"notifications": {"id", "batch_id", "idempotency_key", "request_hash", "channel", "recipient", "content",
			"template_id", "template_vars", "priority", "status", "attempts", "max_attempts", "next_attempt_at",
			"scheduled_at", "locked_until", "lease_token", "batch_position", "provider_message_id", "last_error",
			"correlation_id", "trace_parent", "created_at", "updated_at", "sent_at"},
		"notification_attempts": {"id", "notification_id", "seq", "attempt_no", "started_at", "finished_at", "http_status", "error", "duration_ms"},
		"batches":               {"id", "idempotency_key", "request_hash", "total", "created_at"},
	}
	for table, names := range cols {
		for _, c := range names {
			var n int
			err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`, table, c).Scan(&n)
			if err != nil || n != 1 {
				t.Errorf("missing column %s.%s (err=%v)", table, c, err)
			}
		}
	}
	for _, idx := range []string{"notifications_claim_idx", "notifications_reaper_idx", "notifications_created_idx",
		"notifications_status_created_idx", "notifications_channel_created_idx", "notifications_batch_position_uq"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname=$1`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("missing index %s (err=%v)", idx, err)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	pool := testutil.MigratedPool(t)
	ctx := context.Background()
	ins := `INSERT INTO notifications (channel, recipient, content, priority, correlation_id, idempotency_key, status) VALUES ($1,'r','c',$2,'x',$3,$4)`

	bad := []struct {
		name   string
		ch     string
		prio   int
		key    any
		status string
	}{
		{"bad channel", "fax", 1, nil, "pending"},
		{"bad priority", "sms", 5, nil, "pending"},
		{"bad status", "sms", 1, nil, "bogus"},
	}
	for _, c := range bad {
		if _, err := pool.Exec(ctx, ins, c.ch, c.prio, c.key, c.status); err == nil {
			t.Errorf("%s: expected constraint error", c.name)
		}
	}
	// two NULL keys are fine, a repeated key is not
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, ins, "sms", 1, nil, "pending"); err != nil {
			t.Fatalf("null key insert %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, ins, "sms", 1, "k", "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, ins, "sms", 1, "k", "pending"); err == nil {
		t.Error("duplicate idempotency_key must fail")
	}
	// unique (batch_id, batch_position)
	var batchID string
	if err := pool.QueryRow(ctx, `INSERT INTO batches (total) VALUES (2) RETURNING id::text`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	pos := `INSERT INTO notifications (batch_id, batch_position, channel, recipient, content, priority, correlation_id) VALUES ($1::uuid,$2,'sms','r','c',1,'x')`
	if _, err := pool.Exec(ctx, pos, batchID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, pos, batchID, 0); err == nil {
		t.Error("duplicate (batch_id, batch_position) must fail")
	}
}
