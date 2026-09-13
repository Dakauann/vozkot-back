package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/database"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
)

// TestTransactionsNeverWaitForASecondConnection pins the property that keeps
// the API alive under a full pool: a transaction holding a connection must
// never need ANOTHER connection to run its next statement.
//
// The load harness found the failure this guards against. With every pooled
// connection held by a checkout transaction, each of those transactions sat
// on BEGIN forever, waiting for a statement to be prepared — and the prepare
// was waiting for a free connection, which none of them would release until
// the statement ran. Zero orders in six minutes, nothing blocked at the
// database, and an API that would do the same the first time concurrency
// exceeded DB_MAX_OPEN_CONNS.
//
// The scenario is reproduced on purpose with a pool of two: two transactions
// take both connections and pause; a caller outside any transaction runs the
// same lookup and, with the pool full, has to wait; the transactions then run
// that lookup. Whatever the caller waits for must not depend on them.
func TestTransactionsNeverWaitForASecondConnection(t *testing.T) {
	testsupport.Database(t) // migrated, or skipped with the start command

	ctx := context.Background()
	cfg := testsupport.DatabaseConfig(t)
	cfg.MaxOpenConns, cfg.MaxIdleConns = 2, 2
	db, err := database.NewApplicationDatabase(ctx, cfg)
	if err != nil {
		t.Fatalf("NewApplicationDatabase() error = %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	lookup := func(handle *gorm.DB) error {
		_, err := ticketRepository.NewTicketRepository(handle).GetByID(ctx, "tkt_never_exists")
		if errors.Is(err, ticketdomain.ErrNotFound) {
			return nil
		}
		return err
	}

	// A transaction runs the lookup first, so any statement cache now holds
	// an entry born inside a transaction.
	if err := db.Transaction(func(tx *gorm.DB) error { return lookup(tx) }); err != nil {
		t.Fatalf("priming transaction: %v", err)
	}

	began := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 3)
	for i := 0; i < 2; i++ {
		go func() {
			done <- db.Transaction(func(tx *gorm.DB) error {
				began <- struct{}{}
				<-release
				return lookup(tx)
			})
		}()
	}
	<-began
	<-began // both connections are now held by transactions

	// Outside any transaction, the same lookup: with the pool full it waits
	// for a connection, and it must be able to get one once the transactions
	// finish — which they can only do if they do not wait on it.
	go func() { done <- lookup(db) }()
	time.Sleep(200 * time.Millisecond)
	close(release)

	deadline := time.After(10 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("participant %d: %v", i, err)
			}
		case <-deadline:
			t.Fatalf("deadlock: transactions holding the whole pool are waiting for a connection that only they can free")
		}
	}
}
