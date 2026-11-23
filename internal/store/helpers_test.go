package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.MigratedPool(t)
	return store.New(pool), pool
}

func recipientFor(ch domain.Channel) string {
	switch ch {
	case domain.ChannelEmail:
		return "a@example.com"
	case domain.ChannelPush:
		return "tok123"
	}
	return "+905551234567"
}

func sample(ch domain.Channel, content string) domain.NewNotification {
	return domain.NewNotification{Channel: ch, Recipient: recipientFor(ch), Content: content, Priority: domain.PriorityNormal}
}

func mustCreate(t *testing.T, s *store.Store, ch domain.Channel, content string) domain.Notification {
	t.Helper()
	res, err := s.CreateNotification(context.Background(), store.CreateParams{New: sample(ch, content), CorrelationID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return res.Notification
}
