// Command migrate applies the database migrations and exits.
package main

import (
	"os"

	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func main() {
	log := observability.NewLogger(os.Stdout, "info")
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	if err := store.Migrate(dsn); err != nil {
		// Never print err.Error() or the DSN: either can carry credentials.
		log.Error("migration failed", observability.ErrAttrs(err)...)
		os.Exit(1)
	}
	log.Info("migrations applied")
}
