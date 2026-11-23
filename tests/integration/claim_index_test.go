package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

// planNode is the subset of an EXPLAIN (FORMAT JSON) node this test reads.
type planNode struct {
	NodeType        string     `json:"Node Type"`
	IndexName       string     `json:"Index Name"`
	ActualRows      float64    `json:"Actual Rows"`
	ActualLoops     float64    `json:"Actual Loops"`
	RemovedByFilter float64    `json:"Rows Removed by Filter"`
	RemovedRecheck  float64    `json:"Rows Removed by Index Recheck"`
	Plans           []planNode `json:"Plans"`
}

// walk visits n and every descendant.
func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// TestClaimUsesIndexOnFutureBacklog (spec R8): with a mostly future backlog the
// real claim probe must use notifications_claim_idx and examine only a few rows,
// never scan the future backlog. Index use is observed, never forced.
func TestClaimUsesIndexOnFutureBacklog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := testutil.MigratedPool(t)

	// 20000 future rows (1h to 30d ahead) and 50 due rows, one channel and priority.
	if _, err := pool.Exec(ctx, `
		INSERT INTO notifications (channel, recipient, content, priority, correlation_id, next_attempt_at)
		SELECT 'sms', '+905551234567', 'c', 1, 'r8',
		       now() + interval '1 hour' + (random() * interval '29 days 23 hours')
		FROM generate_series(1, 20000)`); err != nil {
		t.Fatalf("seed future: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notifications (channel, recipient, content, priority, correlation_id, next_attempt_at)
		SELECT 'sms', '+905551234567', 'c', 1, 'r8', now() - interval '1 minute'
		FROM generate_series(1, 50)`); err != nil {
		t.Fatalf("seed due: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE notifications`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var token string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&token); err != nil {
		t.Fatal(err)
	}

	// EXPLAIN ANALYZE really executes the UPDATE, so it runs in a rolled back transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+store.ClaimSQL,
		"sms", int16(1), 20, token).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse plan: %v", err)
	}

	usesClaimIdx := false
	examined := 0.0
	doc[0].Plan.walk(func(n planNode) {
		if n.IndexName == "notifications_claim_idx" {
			usesClaimIdx = true
		}
		if strings.Contains(n.NodeType, "Scan") {
			loops := n.ActualLoops
			if loops < 1 {
				loops = 1
			}
			examined += n.ActualRows*loops + n.RemovedByFilter + n.RemovedRecheck
		}
	})
	if !usesClaimIdx {
		t.Fatalf("plan does not use notifications_claim_idx: %s", raw)
	}
	if examined >= 500 {
		t.Fatalf("claim probe examined %.0f rows, want under 500 (future backlog scanned): %s", examined, raw)
	}
}
