package report

import (
	"context"
	"sync"
	"testing"
	"time"

	domain "vozkot/domain/report"
	ticketdomain "vozkot/domain/ticket"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
)

// Two organisers, three events, one shared database.
//
// The fixture exists to make the failure this feature could have, one
// organiser's portfolio including another's sales, impossible to pass by
// accident. Everything is seeded with unique ids so the suite can run beside
// whatever else is in the development database.
type portfolio struct {
	mine, theirs       string
	firstEvent         string
	secondEvent        string
	theirEvent         string
	mineNet, theirsNet int64
	mineOrders         int
}

func seedPortfolio(t *testing.T) (*ReportRepository, portfolio) {
	t.Helper()
	db := testsupport.Database(t)
	p := portfolio{
		mine:   testsupport.Unique("usr"),
		theirs: testsupport.Unique("usr"),
	}
	for _, id := range []string{p.mine, p.theirs} {
		if err := db.Exec(`
			INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
			VALUES (?, 'Report Test', ?, 'x', 'user', 0, NOW(), NOW())`,
			id, id+"@vozkot.test").Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	p.firstEvent = testsupport.SeedEvent(t, db, p.mine)
	p.secondEvent = testsupport.SeedEvent(t, db, p.mine)
	p.theirEvent = testsupport.SeedEvent(t, db, p.theirs)

	// A real tier per event, because order_items carries a foreign key to one.
	tiers := map[string]string{}
	for _, eventID := range []string{p.firstEvent, p.secondEvent, p.theirEvent} {
		tier, err := ticketdomain.New(testsupport.Unique("tkt"), p.mine, ticketdomain.Draft{
			EventID: eventID, Title: "Pista", PriceCents: 10_000,
			Quantity: 500, Status: ticketdomain.StatusOnSale,
		}, time.Now())
		if err != nil {
			t.Fatalf("build tier: %v", err)
		}
		if err := ticketRepository.NewTicketRepository(db).Create(context.Background(), tier); err != nil {
			t.Fatalf("create tier: %v", err)
		}
		tiers[eventID] = tier.ID
	}

	// buyer_age_years is the column the age breakdown groups on, frozen onto
	// the order at purchase. See domain/order.
	order := func(eventID string, subtotal int64, age int, uf string, quantity int) string {
		id := testsupport.Unique("ord")
		if err := db.Exec(`
			INSERT INTO orders (id, event_id, buyer_id, buyer_name, buyer_email, buyer_document,
			                    buyer_age_years, buyer_gender, buyer_uf, buyer_city,
			                    subtotal_cents, service_fee_cents, total_cents, currency,
			                    status, paid_at, hold_expires_at, refund_policy_version,
			                    created_at, updated_at)
			VALUES (?, ?, ?, 'Maria', 'maria@exemplo.com.br', '12345678909',
			        ?, 'female', ?, 'Natal', ?, 0, ?, 'BRL',
			        'paid', NOW(), NOW(), 1, NOW(), NOW())`,
			id, eventID, testsupport.Unique("usr"), age, uf, subtotal, subtotal).Error; err != nil {
			t.Fatalf("seed order: %v", err)
		}
		if err := db.Exec(`
			INSERT INTO order_items (id, order_id, ticket_id, ticket_title, quantity,
			                         unit_price_cents, total_cents, unit_fee_cents, fee_cents,
			                         created_at)
			VALUES (?, ?, ?, 'Pista', ?, ?, ?, 0, 0, NOW())`,
			testsupport.Unique("oi"), id, tiers[eventID], quantity,
			subtotal/int64(quantity), subtotal).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
		return id
	}

	// Mine: two events, three orders, two age bands.
	order(p.firstEvent, 10_000, 25, "RN", 2)
	order(p.firstEvent, 20_000, 26, "RN", 4)
	order(p.secondEvent, 30_000, 41, "SP", 1)
	p.mineNet, p.mineOrders = 60_000, 3
	// Theirs: one order that must never appear in mine.
	order(p.theirEvent, 99_000, 30, "BA", 9)
	p.theirsNet = 99_000

	t.Cleanup(func() {
		db.Exec(`DELETE FROM order_items WHERE order_id IN
			(SELECT id FROM orders WHERE event_id IN (?, ?, ?))`,
			p.firstEvent, p.secondEvent, p.theirEvent)
		db.Exec("DELETE FROM orders WHERE event_id IN (?, ?, ?)",
			p.firstEvent, p.secondEvent, p.theirEvent)
		db.Exec("DELETE FROM tickets WHERE event_id IN (?, ?, ?)", p.firstEvent, p.secondEvent, p.theirEvent)
		db.Exec("DELETE FROM events WHERE id IN (?, ?, ?)", p.firstEvent, p.secondEvent, p.theirEvent)
		db.Exec("DELETE FROM users WHERE id IN (?, ?)", p.mine, p.theirs)
	})
	return NewReportRepository(db), p
}

// The portfolio is every event the organiser owns, and nothing else.
func TestAPortfolioCoversEveryEventOfOneOrganiserAndNoOneElses(t *testing.T) {
	repository, p := seedPortfolio(t)
	ctx := context.Background()

	mine, err := repository.Sales(ctx, domain.OrganiserScope(p.mine))
	if err != nil {
		t.Fatalf("Sales(organiser): %v", err)
	}
	if mine.Totals.NetCents != p.mineNet {
		t.Fatalf("net = %d, want %d: the portfolio did not span both events",
			mine.Totals.NetCents, p.mineNet)
	}
	if mine.Totals.Orders != p.mineOrders {
		t.Errorf("orders = %d, want %d", mine.Totals.Orders, p.mineOrders)
	}
	// The other organiser's order is the one that must never appear.
	if mine.Totals.NetCents >= p.mineNet+p.theirsNet {
		t.Fatal("another organiser's sales leaked into this portfolio")
	}
	theirs, err := repository.Sales(ctx, domain.OrganiserScope(p.theirs))
	if err != nil {
		t.Fatalf("Sales(other organiser): %v", err)
	}
	if theirs.Totals.NetCents != p.theirsNet {
		t.Errorf("their net = %d, want %d", theirs.Totals.NetCents, p.theirsNet)
	}
}

// The portfolio and the sum of its events are computed by the same SQL with one
// clause swapped, and this is the test that keeps them agreeing. Two sets of
// near-identical queries is how a total starts disagreeing with its parts.
func TestAPortfolioEqualsTheSumOfItsEvents(t *testing.T) {
	repository, p := seedPortfolio(t)
	ctx := context.Background()

	whole, err := repository.Sales(ctx, domain.OrganiserScope(p.mine))
	if err != nil {
		t.Fatalf("Sales(organiser): %v", err)
	}
	var net, refunded int64
	var orders, tickets int
	for _, eventID := range []string{p.firstEvent, p.secondEvent} {
		part, err := repository.Sales(ctx, domain.EventScope(eventID))
		if err != nil {
			t.Fatalf("Sales(%s): %v", eventID, err)
		}
		net += part.Totals.NetCents
		refunded += part.Totals.RefundedCents
		orders += part.Totals.Orders
		tickets += part.Totals.Tickets
	}
	if whole.Totals.NetCents != net {
		t.Errorf("portfolio net %d != sum of events %d", whole.Totals.NetCents, net)
	}
	if whole.Totals.Orders != orders {
		t.Errorf("portfolio orders %d != sum of events %d", whole.Totals.Orders, orders)
	}
	if whole.Totals.Tickets != tickets {
		t.Errorf("portfolio tickets %d != sum of events %d", whole.Totals.Tickets, tickets)
	}
	if whole.Totals.RefundedCents != refunded {
		t.Errorf("portfolio refunded %d != sum of events %d", whole.Totals.RefundedCents, refunded)
	}
}

// The demographic question this feature was asked for: who is buying, across
// everything the organiser sells.
func TestAPortfolioAnswersWhichAgeBuysMostAcrossEveryEvent(t *testing.T) {
	repository, p := seedPortfolio(t)

	sales, err := repository.Sales(context.Background(), domain.OrganiserScope(p.mine))
	if err != nil {
		t.Fatalf("Sales: %v", err)
	}
	byAge := map[string]domain.Slice{}
	for _, slice := range sales.ByAge {
		byAge[slice.Key] = slice
	}
	// Ages 25 and 26 both fall in 24-28 and come from two different EVENTS, so
	// this only holds if the portfolio really spans them.
	band := byAge[string(domain.Age24To28)]
	if band.Orders != 2 || band.Tickets != 6 {
		t.Fatalf("24-28 = %d orders / %d tickets, want 2 and 6 across both events",
			band.Orders, band.Tickets)
	}
	if band.NetCents != 30_000 {
		t.Errorf("24-28 net = %d, want 30000", band.NetCents)
	}
	// And the 41-year-old from the second event is in their own band.
	if older := byAge[string(domain.Age39To43)]; older.Orders != 1 || older.NetCents != 30_000 {
		t.Errorf("39-43 = %+v, want the one order worth 30000", older)
	}
	// Every band the domain knows is a band the SQL can produce; nothing landed
	// in a bucket the client would not recognise.
	known := map[string]bool{}
	for _, bracket := range domain.AgeBrackets() {
		known[string(bracket)] = true
	}
	for _, slice := range sales.ByAge {
		if !known[slice.Key] {
			t.Errorf("age bucket %q is not one the domain names", slice.Key)
		}
	}
}

// An empty or ambiguous scope must read NOTHING.
//
// The failure this guards is the worst one available here: a report that
// silently covered the whole platform because an id arrived empty would hand
// whoever asked first every organiser's revenue.
func TestAnUnscopedReportReadsNothingRatherThanEverything(t *testing.T) {
	repository, p := seedPortfolio(t)
	ctx := context.Background()

	for name, scope := range map[string]domain.Scope{
		"empty":     {},
		"both set":  {EventID: p.firstEvent, OrganiserID: p.mine},
		"blank ids": {EventID: "   ", OrganiserID: "  "},
	} {
		sales, err := repository.Sales(ctx, scope)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sales.Totals.Orders != 0 || sales.Totals.NetCents != 0 {
			t.Fatalf("%s scope returned %d orders worth %d",
				name, sales.Totals.Orders, sales.Totals.NetCents)
		}
		if len(sales.ByAge) != 0 || len(sales.ByTier) != 0 {
			t.Fatalf("%s scope returned breakdowns", name)
		}
	}
}

// A reporting screen must not be able to take the whole connection pool.
//
// Six queries per report, and a dashboard that several people open at once is
// ordinary. This runs the portfolio concurrently to prove the reads are
// consistent under contention and that nothing deadlocks against the pool the
// checkout traffic shares.
func TestConcurrentPortfolioReadsAgreeAndDoNotExhaustThePool(t *testing.T) {
	repository, p := seedPortfolio(t)
	ctx := context.Background()

	const readers = 12
	var wg sync.WaitGroup
	results := make(chan int64, readers)
	failures := make(chan error, readers)
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sales, err := repository.Sales(ctx, domain.OrganiserScope(p.mine))
			if err != nil {
				failures <- err
				return
			}
			results <- sales.Totals.NetCents
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)

	for err := range failures {
		t.Fatalf("a concurrent report failed: %v", err)
	}
	seen := 0
	for net := range results {
		seen++
		if net != p.mineNet {
			t.Fatalf("a concurrent read saw %d, want %d", net, p.mineNet)
		}
	}
	if seen != readers {
		t.Fatalf("%d of %d readers returned", seen, readers)
	}
}
