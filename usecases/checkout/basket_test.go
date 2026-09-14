package checkout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	orderdomain "vozkot/domain/order"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	queueUsecase "vozkot/usecases/queue"
)

// An order covering several tiers of one night, and the two-phase hold that
// takes those tiers off the shelf before the buyer has typed anything.
//
// Against a real PostgreSQL, like everything else in this package. The
// properties under test are the ones that only exist in the database: that a
// basket which cannot be fully reserved leaves no partial hold behind, and that
// two buyers reaching for the same two tiers at once cannot deadlock.

// basket is a box office with several tiers on one event, plus a tier on a
// second event to test that a basket cannot span both.
type basket struct {
	db      *gorm.DB
	service *Service
	orders  orderdomain.Repository
	tickets ticketdomain.Repository
	jobs    queuedomain.Queue

	ownerID string
	eventID string
	// pista and camarote belong to eventID; elsewhere belongs to another event.
	pista     string
	camarote  string
	elsewhere string
}

func newBasket(t *testing.T, pistaStock, camaroteStock int) *basket {
	t.Helper()
	return newBasketWithLimits(t, pistaStock, camaroteStock, orderdomain.HoldLimits{})
}

func newBasketWithLimits(t *testing.T, pistaStock, camaroteStock int, limits orderdomain.HoldLimits) *basket {
	t.Helper()
	db := testsupport.Database(t)
	ctx := context.Background()

	tickets := ticketRepository.NewTicketRepository(db)
	orders := orderRepository.NewOrderRepository(db)
	jobs := queueRepository.NewJobRepository(db)

	ownerID := seedUser(t, db)
	eventID := testsupport.SeedEvent(t, db, ownerID)
	otherEventID := testsupport.SeedEvent(t, db, ownerID)

	tier := func(eventID, title string, price int64, stock int) string {
		t.Helper()
		item, err := ticketdomain.New(testsupport.Unique("tkt"), ownerID, ticketdomain.Draft{
			EventID:    eventID,
			Title:      title,
			PriceCents: price,
			Quantity:   stock,
			Status:     ticketdomain.StatusOnSale,
		}, time.Now())
		if err != nil {
			t.Fatalf("build %s: %v", title, err)
		}
		if err := tickets.Create(ctx, item); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return item.ID
	}

	pista := tier(eventID, "Pista", 9000, pistaStock)
	camarote := tier(eventID, "Camarote", 24000, camaroteStock)
	elsewhere := tier(otherEventID, "Outra noite", 5000, 10)

	t.Cleanup(func() {
		ids := []string{pista, camarote, elsewhere}
		db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN (SELECT order_id FROM order_items WHERE ticket_id IN ?)", ids)
		db.Exec("DELETE FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE ticket_id IN ?)", ids)
		db.Exec("DELETE FROM tickets WHERE id IN ?", ids)
		db.Exec("DELETE FROM users WHERE id = ?", ownerID)
	})

	return &basket{
		db:      db,
		service: NewService(uow.NewRunner(db), orders, tickets, queueUsecase.NewDispatcher(nil), 30*time.Minute, 10*time.Minute, limits),
		orders:  orders, tickets: tickets, jobs: jobs,
		ownerID: ownerID, eventID: eventID,
		pista: pista, camarote: camarote, elsewhere: elsewhere,
	}
}

// reserve opens a cart hold: no buyer details, no charge.
func (b *basket) reserve(buyerID string, lines ...orderdomain.DraftItem) (*orderdomain.Order, error) {
	if buyerID == "" {
		buyerID = b.ownerID
	}
	return b.service.Start(context.Background(), StartInput{
		Items:          lines,
		BuyerID:        buyerID,
		IdempotencyKey: testsupport.Unique("key"),
	})
}

func (b *basket) stock(t *testing.T, ticketID string) ticketdomain.Ticket {
	t.Helper()
	item, err := b.tickets.GetByID(context.Background(), ticketID)
	if err != nil {
		t.Fatalf("read tier %s: %v", ticketID, err)
	}
	return *item
}

func (b *basket) jobTypes(t *testing.T, orderID string) []string {
	t.Helper()
	var types []string
	err := b.db.Raw("SELECT type FROM jobs WHERE payload->>'orderId' = ? ORDER BY type", orderID).Scan(&types).Error
	if err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	return types
}

func line(ticketID string, quantity int) orderdomain.DraftItem {
	return orderdomain.DraftItem{TicketID: ticketID, Quantity: quantity}
}

// TestBasketReservesEveryTierOfOneEvent is the feature the buy panel has been
// promising and the API could not keep: picking two tiers bought one of them
// and silently dropped the rest.
func TestBasketReservesEveryTierOfOneEvent(t *testing.T) {
	b := newBasket(t, 10, 10)

	item, err := b.reserve("", line(b.pista, 2), line(b.camarote, 1))

	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	if len(item.Items) != 2 {
		t.Fatalf("items = %d, want 2: the second tier must not be dropped", len(item.Items))
	}
	if item.EventID != b.eventID {
		t.Fatalf("event = %q, want %q", item.EventID, b.eventID)
	}
	if item.TotalQuantity() != 3 {
		t.Fatalf("quantity = %d, want 3", item.TotalQuantity())
	}
	// 2 x 9000 + 1 x 24000. Priced from the tier rows, never from the request.
	if item.TotalCents != 42000 {
		t.Fatalf("total = %d, want 42000", item.TotalCents)
	}
	if got := b.stock(t, b.pista).Reserved; got != 2 {
		t.Fatalf("pista reserved = %d, want 2", got)
	}
	if got := b.stock(t, b.camarote).Reserved; got != 1 {
		t.Fatalf("camarote reserved = %d, want 1", got)
	}
}

// TestBasketRecordsTheTitleAndPriceAtPurchase: a receipt has to keep saying what
// was bought after the organiser renames or re-prices the tier.
func TestBasketRecordsTheTitleAndPriceAtPurchase(t *testing.T) {
	b := newBasket(t, 10, 10)

	item, err := b.reserve("", line(b.camarote, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}

	tier, err := b.tickets.GetByID(context.Background(), b.camarote)
	if err != nil {
		t.Fatalf("read tier: %v", err)
	}
	if err := tier.Apply(ticketdomain.Draft{
		EventID: b.eventID, Title: "Camarote Premium", PriceCents: 99000, Quantity: 10,
		Status: ticketdomain.StatusOnSale,
	}, time.Now()); err != nil {
		t.Fatalf("rename tier: %v", err)
	}
	if err := b.tickets.Update(context.Background(), tier); err != nil {
		t.Fatalf("save tier: %v", err)
	}

	stored, err := b.orders.GetByID(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if stored.Items[0].TicketTitle != "Camarote" {
		t.Fatalf("title = %q, want the name at purchase", stored.Items[0].TicketTitle)
	}
	if stored.Items[0].UnitPriceCents != 24000 {
		t.Fatalf("unit price = %d, want the price at purchase", stored.Items[0].UnitPriceCents)
	}
	if stored.TotalCents != 24000 {
		t.Fatalf("total = %d, want the total at purchase", stored.TotalCents)
	}
}

// TestBasketRefusesTwoEvents: one order is one night, because the hold window,
// the door time and the venue on the receipt all belong to a single one.
func TestBasketRefusesTwoEvents(t *testing.T) {
	b := newBasket(t, 10, 10)

	_, err := b.reserve("", line(b.pista, 1), line(b.elsewhere, 1))

	if !errors.Is(err, orderdomain.ErrMultipleEvents) {
		t.Fatalf("reserve() error = %v, want %v", err, orderdomain.ErrMultipleEvents)
	}
	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0: a refused basket must hold nothing", got)
	}
}

// TestBasketIsAllOrNothing is the property a per-line reservation would break.
//
// The buyer asked for two Pista and three Camarote; only one Camarote is left.
// Committing the Pista anyway would sell them half an evening they did not ask
// for, and would take stock off the shelf for an order that failed.
func TestBasketIsAllOrNothing(t *testing.T) {
	b := newBasket(t, 10, 1)

	_, err := b.reserve("", line(b.pista, 2), line(b.camarote, 3))

	if !errors.Is(err, ticketdomain.ErrInsufficientStock) {
		t.Fatalf("reserve() error = %v, want %v", err, ticketdomain.ErrInsufficientStock)
	}
	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0: the whole basket rolls back", got)
	}
	if got := b.stock(t, b.camarote).Reserved; got != 0 {
		t.Fatalf("camarote reserved = %d, want 0", got)
	}
}

// TestBasketMergesARepeatedTier: two taps of "+" that raced mean three of that
// tier, not two holds on it. The unique index on (order_id, ticket_id) is what
// would otherwise reject the insert.
func TestBasketMergesARepeatedTier(t *testing.T) {
	b := newBasket(t, 10, 10)

	item, err := b.reserve("", line(b.pista, 2), line(b.pista, 1))

	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	if len(item.Items) != 1 {
		t.Fatalf("items = %d, want 1 merged line", len(item.Items))
	}
	if item.Items[0].Quantity != 3 {
		t.Fatalf("quantity = %d, want 3", item.Items[0].Quantity)
	}
	if got := b.stock(t, b.pista).Reserved; got != 3 {
		t.Fatalf("reserved = %d, want 3", got)
	}
}

// TestBasketCapsTheWholeOrderNotEachLine: a cap that loosens itself the more
// the organiser subdivides the house is not a cap.
func TestBasketCapsTheWholeOrderNotEachLine(t *testing.T) {
	b := newBasket(t, 50, 50)

	_, err := b.reserve("",
		line(b.pista, orderdomain.MaxQuantityPerOrder),
		line(b.camarote, 1),
	)

	if !errors.Is(err, orderdomain.ErrInvalidQuantity) {
		t.Fatalf("reserve() error = %v, want %v", err, orderdomain.ErrInvalidQuantity)
	}
	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0", got)
	}
}

// TestPerTierCapAppliesToEveryLine.
//
// The regression this guards is specific: a cap that checked only the basket's
// first line would be a cap the buyer chooses the strength of by reordering
// their own request.
func TestPerTierCapAppliesToEveryLine(t *testing.T) {
	b := newBasketWithLimits(t, 50, 50, orderdomain.HoldLimits{TicketsPerTier: 2})

	// The first line is inside the cap; the second is not.
	_, err := b.reserve("", line(b.pista, 1), line(b.camarote, 3))

	if !errors.Is(err, orderdomain.ErrTooManyHeldTickets) {
		t.Fatalf("reserve() error = %v, want %v", err, orderdomain.ErrTooManyHeldTickets)
	}
	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0", got)
	}
}

// TestPerTierCapCountsHoldsAcrossOrders: the cap is about what one account is
// sitting on, not about one request.
func TestPerTierCapCountsHoldsAcrossOrders(t *testing.T) {
	b := newBasketWithLimits(t, 50, 50, orderdomain.HoldLimits{TicketsPerTier: 3})

	if _, err := b.reserve("", line(b.pista, 2)); err != nil {
		t.Fatalf("first reserve() error = %v", err)
	}
	_, err := b.reserve("", line(b.pista, 2), line(b.camarote, 1))

	if !errors.Is(err, orderdomain.ErrTooManyHeldTickets) {
		t.Fatalf("second reserve() error = %v, want %v", err, orderdomain.ErrTooManyHeldTickets)
	}
	if got := b.stock(t, b.camarote).Reserved; got != 0 {
		t.Fatalf("camarote reserved = %d, want 0: the refused basket holds nothing", got)
	}
}

// TestReserveHoldsStockWithoutAskingForMoney is the first half of the two-phase
// hold: the tickets come off the shelf when the buyer reaches the form, and
// nothing is charged until they submit it.
func TestReserveHoldsStockWithoutAskingForMoney(t *testing.T) {
	b := newBasket(t, 10, 10)
	before := time.Now()

	item, err := b.reserve("", line(b.pista, 2))

	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	if item.Confirmed {
		t.Fatal("order is confirmed, want a cart hold awaiting the buyer's details")
	}
	if got := b.stock(t, b.pista).Reserved; got != 2 {
		t.Fatalf("reserved = %d, want 2: stock must be held before the form is filled in", got)
	}
	// The cart window, not the payment window.
	window := item.HoldExpiresAt.Sub(before)
	if window > 11*time.Minute || window < 9*time.Minute {
		t.Fatalf("hold window = %v, want about the 10 minute cart TTL", window)
	}
	if types := b.jobTypes(t, item.ID); len(types) != 1 || types[0] != queuedomain.TypeExpireHolds {
		t.Fatalf("jobs = %v, want only an expiry; a charge for an unconfirmed basket asks for money nobody offered", types)
	}
}

// TestConfirmExtendsTheHoldAndAsksForTheCharge is the second half.
func TestConfirmExtendsTheHoldAndAsksForTheCharge(t *testing.T) {
	b := newBasket(t, 10, 10)
	item, err := b.reserve("", line(b.pista, 2), line(b.camarote, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	cartExpiry := item.HoldExpiresAt

	confirmed, err := b.service.Confirm(context.Background(), ConfirmInput{
		OrderID:       item.ID,
		BuyerID:       b.ownerID,
		BuyerName:     "Maria Souza",
		BuyerEmail:    "maria@exemplo.com.br",
		BuyerDocument: "12345678909",
	})

	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if !confirmed.Confirmed {
		t.Fatal("order is not confirmed")
	}
	if !confirmed.HoldExpiresAt.After(cartExpiry) {
		t.Fatalf("hold still ends at %v, want it extended past the cart window %v",
			confirmed.HoldExpiresAt, cartExpiry)
	}
	if confirmed.BuyerEmail != "maria@exemplo.com.br" {
		t.Fatalf("email = %q, want the confirmed buyer", confirmed.BuyerEmail)
	}
	// Stock did not move: confirming is not a second reservation.
	if got := b.stock(t, b.pista).Reserved; got != 2 {
		t.Fatalf("pista reserved = %d, want 2 unchanged", got)
	}
	types := b.jobTypes(t, item.ID)
	if len(types) != 3 || types[0] != queuedomain.TypeCreateCharge {
		t.Fatalf("jobs = %v, want a charge plus one expiry per window", types)
	}
}

// TestConfirmingTwiceDoesNotExtendTwice.
//
// An extension per press would be a way to hold stock forever by pressing a
// button, which is exactly the abuse the short cart window exists to prevent.
func TestConfirmingTwiceDoesNotExtendTwice(t *testing.T) {
	b := newBasket(t, 10, 10)
	item, err := b.reserve("", line(b.pista, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}

	details := ConfirmInput{
		OrderID: item.ID, BuyerID: b.ownerID,
		BuyerName: "Maria Souza", BuyerEmail: "maria@exemplo.com.br", BuyerDocument: "12345678909",
	}
	first, err := b.service.Confirm(context.Background(), details)
	if err != nil {
		t.Fatalf("first Confirm() error = %v", err)
	}
	// A corrected email on the second pass: saved, but not paid for with more
	// time.
	details.BuyerEmail = "maria.souza@exemplo.com.br"
	second, err := b.service.Confirm(context.Background(), details)
	if err != nil {
		t.Fatalf("second Confirm() error = %v", err)
	}

	// Compared at the precision PostgreSQL actually keeps. The first value is
	// still the in-memory one Go built, at 100ns; the second has been through a
	// timestamptz column, which stores microseconds. Comparing them exactly
	// fails on a truncation rather than on the behaviour under test.
	if !second.HoldExpiresAt.Truncate(time.Microsecond).Equal(first.HoldExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("hold moved from %v to %v on a second confirm", first.HoldExpiresAt, second.HoldExpiresAt)
	}
	if second.BuyerEmail != "maria.souza@exemplo.com.br" {
		t.Fatalf("email = %q, want the correction saved", second.BuyerEmail)
	}
	charges := 0
	for _, jobType := range b.jobTypes(t, item.ID) {
		if jobType == queuedomain.TypeCreateCharge {
			charges++
		}
	}
	if charges != 1 {
		t.Fatalf("charge jobs = %d, want exactly 1", charges)
	}
}

// TestConfirmRefusesAnotherBuyersOrder: an order may only be confirmed by the
// account that opened it.
func TestConfirmRefusesAnotherBuyersOrder(t *testing.T) {
	b := newBasket(t, 10, 10)
	item, err := b.reserve("", line(b.pista, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	intruder := seedUser(t, b.db)
	t.Cleanup(func() { b.db.Exec("DELETE FROM users WHERE id = ?", intruder) })

	_, err = b.service.Confirm(context.Background(), ConfirmInput{
		OrderID: item.ID, BuyerID: intruder,
		BuyerName: "Nao Eu", BuyerEmail: "naoeu@exemplo.com.br",
	})

	if !errors.Is(err, orderdomain.ErrNotFound) {
		t.Fatalf("Confirm() error = %v, want %v", err, orderdomain.ErrNotFound)
	}
}

// TestConfirmRefusesALapsedCart: the window closed while the form was open, so
// there is nothing left to extend.
func TestConfirmRefusesALapsedCart(t *testing.T) {
	b := newBasket(t, 10, 10)
	item, err := b.reserve("", line(b.pista, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	if err := b.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	_, err = b.service.Confirm(context.Background(), ConfirmInput{
		OrderID: item.ID, BuyerID: b.ownerID,
		BuyerName: "Maria Souza", BuyerEmail: "maria@exemplo.com.br",
	})

	if !errors.Is(err, orderdomain.ErrHoldExpired) {
		t.Fatalf("Confirm() error = %v, want %v", err, orderdomain.ErrHoldExpired)
	}
}

// TestExpiringABasketReturnsEveryTier.
func TestExpiringABasketReturnsEveryTier(t *testing.T) {
	b := newBasket(t, 10, 10)
	item, err := b.reserve("", line(b.pista, 2), line(b.camarote, 3))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	if err := b.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	if err := b.service.ExpireHold(context.Background(), item.ID); err != nil {
		t.Fatalf("ExpireHold() error = %v", err)
	}

	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0", got)
	}
	if got := b.stock(t, b.camarote).Reserved; got != 0 {
		t.Fatalf("camarote reserved = %d, want 0: every line comes back, not just the first", got)
	}
}

// TestCancellingABasketReturnsEveryTier.
func TestCancellingABasketReturnsEveryTier(t *testing.T) {
	b := newBasket(t, 10, 10)
	item, err := b.reserve("", line(b.pista, 2), line(b.camarote, 3))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}

	if _, err := b.service.Cancel(context.Background(), item.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0", got)
	}
	if got := b.stock(t, b.camarote).Reserved; got != 0 {
		t.Fatalf("camarote reserved = %d, want 0", got)
	}
}

// TestOppositeBasketsDoNotDeadlock is why NormalizeItems sorts.
//
// Reserving stock takes a row lock per tier. Twenty buyers asking for
// Pista+Camarote while twenty ask for Camarote+Pista is the textbook deadlock:
// each transaction holds what another needs next, and PostgreSQL resolves it by
// killing one of them with SQLSTATE 40P01. Sorting every basket by tier id
// makes the lock order identical for everyone, so the cycle cannot form.
//
// The assertion is that NOTHING fails for a reason other than stock, and that
// the tiers are not oversold; a deadlock would surface as neither.
func TestOppositeBasketsDoNotDeadlock(t *testing.T) {
	const attempts = 24
	b := newBasket(t, attempts, attempts)

	buyers := make([]string, attempts)
	for index := range buyers {
		buyers[index] = seedUser(t, b.db)
	}
	t.Cleanup(func() { b.db.Exec("DELETE FROM users WHERE id IN ?", buyers) })

	var wait sync.WaitGroup
	failures := make(chan error, attempts)
	start := make(chan struct{})

	for index := 0; index < attempts; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			lines := []orderdomain.DraftItem{line(b.pista, 1), line(b.camarote, 1)}
			if index%2 == 1 {
				// The other way round, which is the whole point.
				lines[0], lines[1] = lines[1], lines[0]
			}
			if _, err := b.reserve(buyers[index], lines...); err != nil {
				failures <- err
			}
		}(index)
	}
	close(start)
	wait.Wait()
	close(failures)

	for err := range failures {
		// Running out is a legitimate outcome; a deadlock is not.
		if !errors.Is(err, ticketdomain.ErrInsufficientStock) {
			t.Fatalf("a concurrent basket failed for a reason other than stock: %v", err)
		}
	}
	for name, id := range map[string]string{"pista": b.pista, "camarote": b.camarote} {
		tier := b.stock(t, id)
		if tier.Sold+tier.Reserved > tier.Quantity {
			t.Fatalf("%s oversold: sold %d + reserved %d > capacity %d",
				name, tier.Sold, tier.Reserved, tier.Quantity)
		}
	}
}
