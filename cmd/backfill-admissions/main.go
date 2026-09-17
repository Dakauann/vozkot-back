// Command backfill-admissions issues the tickets that orders paid before the
// door feature shipped never received.
//
// Admissions are minted inside the transaction that crosses an order into
// paid. That is deliberate and is what stops a buyer ever holding a receipt
// with no way through the door — but it also means an order that reached paid
// BEFORE this feature existed has no tickets, and its buyer opens their wallet
// to "no tickets issued for this order yet" for good.
//
// Run it once after deploying the door feature. It is idempotent and reports
// only what it actually minted, so running it twice, or wiring it into a
// release step, costs nothing: an order that already has its tickets keeps
// exactly the codes its buyer was emailed.
//
// Usage:
//
//	go run ./cmd/backfill-admissions            # up to 500 orders
//	go run ./cmd/backfill-admissions -limit 5000
//	go run ./cmd/backfill-admissions -dry-run   # count what is missing
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/gorm"

	orderdomain "vozkot/domain/order"
	"vozkot/infra/config"
	"vozkot/infra/database"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	"vozkot/infra/uow"
	paymentUsecase "vozkot/usecases/payment"
	queueUsecase "vozkot/usecases/queue"
)

func main() {
	limit := flag.Int("limit", 500, "how many paid orders to examine, oldest first")
	dryRun := flag.Bool("dry-run", false, "report how many paid orders have no tickets and change nothing")
	flag.Parse()

	// Best effort, exactly as the other commands do it: a deployed process is
	// configured by its environment and has no .env to read.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}

	// Generous, because this walks orders one transaction at a time and an
	// operator running it over a large box office should not have it cut off
	// half way. It is safe to interrupt and safe to resume.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	db, err := database.NewApplicationDatabase(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	pool, err := db.DB()
	if err != nil {
		log.Fatalf("access connection pool: %v", err)
	}
	defer pool.Close()

	// No migrations. This command reads and writes application data; a data
	// backfill that also changed the schema would be two things an operator
	// cannot roll back separately.

	orders := orderRepository.NewOrderRepository(db)

	if *dryRun {
		missing, examined, err := countMissing(ctx, db, *limit)
		if err != nil {
			log.Fatalf("count orders without tickets: %v", err)
		}
		log.Printf("dry run: %d of %d paid order(s) examined have no tickets", missing, examined)
		return
	}

	// The gateway, the dispatcher and the notifier are not reached by the
	// backfill: it issues tickets for orders that are ALREADY paid, so there is
	// no charge to read and no receipt to send. They are left nil rather than
	// half-built, so that if this command ever grows a call that does need one,
	// it fails loudly here instead of quietly doing nothing in production.
	payments := paymentUsecase.NewService(
		uow.NewRunner(db),
		orders,
		nil,
		queueRepository.NewJobRepository(db),
		queueUsecase.NewDispatcher(nil),
		nil,
	)

	started := time.Now()
	issued, err := payments.BackfillAdmissions(ctx, *limit)
	// The count is reported either way. A failure part way through has still
	// committed the orders it finished, and an operator needs to know that
	// before deciding whether to run it again.
	if err != nil {
		log.Printf("backfill issued %d admission(s) before failing after %s", issued, time.Since(started).Round(time.Millisecond))
		log.Fatalf("backfill admissions: %v", err)
	}
	log.Printf("backfill issued %d admission(s) in %s", issued, time.Since(started).Round(time.Millisecond))
}

// countMissing answers the dry run: how many paid orders hold no tickets.
//
// One query rather than a walk, because a dry run exists to be cheap. It reads
// nothing it would write and takes no locks. The window is the same one the
// backfill itself walks -- oldest paid first, bounded by limit -- so the two
// numbers describe the same set of orders.
func countMissing(ctx context.Context, db *gorm.DB, limit int) (missing int64, examined int64, err error) {
	row := db.WithContext(ctx).Raw(`
		SELECT
			count(*) FILTER (
				WHERE NOT EXISTS (SELECT 1 FROM admissions a WHERE a.order_id = o.id)
			) AS missing,
			count(*) AS examined
		FROM (
			SELECT id FROM orders WHERE status = ? ORDER BY updated_at, id LIMIT ?
		) o`, string(orderdomain.StatusPaid), limit).Row()
	if err := row.Scan(&missing, &examined); err != nil {
		return 0, 0, err
	}
	return missing, examined, nil
}
