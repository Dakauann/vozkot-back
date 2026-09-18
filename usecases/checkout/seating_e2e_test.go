package checkout

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderdomain "vozkot/domain/order"
	seatingdomain "vozkot/domain/seating"
	seatingRepository "vozkot/infra/repositories/seating"
	"vozkot/infra/testsupport"
)

// Reserved seating, against a real PostgreSQL.
//
// It has to be real. The property under test is that two buyers cannot both
// hold FILA K POLTRONA 12, and that property does not live in Go: it lives in
// one conditional UPDATE and the row lock PostgreSQL takes to evaluate it. A
// fake repository here would be testing the fake's mutex.

// seatedHarness is the ordinary harness plus a room: a venue, a published
// layout with one seated section, and that section's seats materialised against
// the harness tier.
type seatedHarness struct {
	*harness
	layoutID  string
	sectionID string
	// seatIDs are the event seats, in row-then-seat order.
	seatIDs []string
	labels  map[string]string
}

// newSeatedHarness lays out `rows` × `perRow` chairs and puts them on sale.
func newSeatedHarness(t *testing.T, rows, perRow int) *seatedHarness {
	t.Helper()
	// Capacity has to cover the chairs: the tier counter is a PROJECTION of the
	// seats, and a tier that could hold fewer than it has chairs would trip its
	// own CHECK constraint the moment the last one sold.
	base := newHarness(t, rows*perRow)
	ctx := context.Background()

	// The room itself comes from testsupport, so the payment package's
	// settlement tests build the same one instead of a second copy whose row
	// letters could drift from this one's.
	room := testsupport.SeedSeatedRoom(t, base.db, base.ownerID, rows, perRow)
	layoutID, sectionID := room.LayoutID, room.SectionID

	seats := seatingRepository.NewSeatRepository(base.db)
	if _, err := seats.Materialise(ctx, seatingdomain.MaterialisePlan{
		EventID:          base.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{testsupport.SeatedRoomCategory: base.ticketID},
	}); err != nil {
		t.Fatalf("materialise: %v", err)
	}

	testsupport.CleanupEventSeats(t, base.db, base.eventID)

	live, err := seats.ListByEvent(ctx, base.eventID, 0)
	if err != nil {
		t.Fatalf("list event seats: %v", err)
	}
	if len(live) != rows*perRow {
		t.Fatalf("materialised %d seats, want %d", len(live), rows*perRow)
	}
	ids := make([]string, 0, len(live))
	labels := make(map[string]string, len(live))
	for _, seat := range live {
		ids = append(ids, seat.ID)
		labels[seat.ID] = seat.Label.String()
	}
	return &seatedHarness{
		harness:   base,
		layoutID:  layoutID,
		sectionID: sectionID,
		seatIDs:   ids,
		labels:    labels,
	}
}

func (h *seatedHarness) buy(seatIDs []string, key string) (*orderdomain.Order, error) {
	return h.service.Start(context.Background(), StartInput{
		Items:          []orderdomain.DraftItem{{TicketID: h.ticketID, SeatIDs: seatIDs}},
		Confirm:        true,
		BuyerID:        h.ownerID,
		BuyerName:      "Maria Souza",
		BuyerEmail:     "maria@exemplo.com.br",
		BuyerDocument:  "12345678909",
		IdempotencyKey: key,
	})
}

func (h *seatedHarness) statusOf(t *testing.T, seatID string) seatingdomain.Status {
	t.Helper()
	var status string
	if err := h.db.Raw("SELECT status FROM event_seats WHERE id = ?", seatID).
		Scan(&status).Error; err != nil {
		t.Fatalf("read seat status: %v", err)
	}
	return seatingdomain.Status(status)
}

// The headline property: one chair, one buyer.
//
// Sixteen goroutines ask for the same seat at the same moment. Not "rarely two"
// and not "usually one": exactly one, every run, decided by the database.
func TestOneSeatGoesToExactlyOneBuyerUnderConcurrency(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	contested := h.seatIDs[9]

	const buyers = 16
	var wait sync.WaitGroup
	wait.Add(buyers)
	won := make([]*orderdomain.Order, buyers)
	failed := make([]error, buyers)

	ready := make(chan struct{})
	for index := 0; index < buyers; index++ {
		go func(slot int) {
			defer wait.Done()
			<-ready
			order, err := h.buy([]string{contested}, testsupport.Unique("race"))
			won[slot], failed[slot] = order, err
		}(index)
	}
	close(ready)
	wait.Wait()

	holders := 0
	for index := 0; index < buyers; index++ {
		if failed[index] == nil && won[index] != nil {
			holders++
			continue
		}
		// Everybody else must lose for a REASON THE BUYER CAN READ, naming the
		// chair. A bare "not enough tickets" would make the picker grey the
		// whole selection.
		var unavailable *SeatsUnavailableError
		if errors.As(failed[index], &unavailable) {
			if len(unavailable.Seats) != 1 || unavailable.Seats[0].ID != contested {
				t.Errorf("loser blamed %v, want exactly the contested seat", unavailable.IDs())
			}
			continue
		}
		// A hold-limit refusal is also legitimate here: sixteen orders from one
		// buyer is exactly what that cap exists to stop.
		if errors.Is(failed[index], orderdomain.ErrTooManyOpenOrders) ||
			errors.Is(failed[index], orderdomain.ErrTooManyHeldTickets) {
			continue
		}
		t.Errorf("buyer %d failed with an unreadable error: %v", index, failed[index])
	}

	if holders != 1 {
		t.Fatalf("%d buyers hold one chair, want exactly 1", holders)
	}
	if status := h.statusOf(t, contested); status != seatingdomain.StatusHeld {
		t.Fatalf("the contested seat is %q, want held", status)
	}
}

// A basket is all or nothing, and the loser is told which chair went.
func TestAPartialSeatClaimCommitsNothing(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	taken := h.seatIDs[2]

	if _, err := h.buy([]string{taken}, testsupport.Unique("first")); err != nil {
		t.Fatalf("first buy: %v", err)
	}

	wanted := []string{h.seatIDs[0], h.seatIDs[1], taken, h.seatIDs[3]}
	_, err := h.buy(wanted, testsupport.Unique("second"))

	var unavailable *SeatsUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("second buy error = %v, want SeatsUnavailableError", err)
	}
	if len(unavailable.Seats) != 1 || unavailable.Seats[0].ID != taken {
		t.Errorf("blamed %v, want only the taken seat", unavailable.IDs())
	}
	// The label, not just the id: this is what a buyer reads.
	if unavailable.Seats[0].Label.String() == "" {
		t.Error("the unavailable seat came back without a label; a buyer cannot act on an id")
	}
	if !errors.Is(err, seatingdomain.ErrSeatsUnavailable) {
		t.Error("the error does not unwrap to ErrSeatsUnavailable, so generic handlers will miss it")
	}

	// And nothing was kept. The three seats it COULD have had are still free,
	// which is the rollback doing its job.
	for _, free := range []string{h.seatIDs[0], h.seatIDs[1], h.seatIDs[3]} {
		if status := h.statusOf(t, free); status != seatingdomain.StatusAvailable {
			t.Errorf("seat %s is %q after a failed basket, want available", h.labels[free], status)
		}
	}
	// The counter agrees: one chair gone, not four.
	stock := h.stock(t)
	if stock.Reserved != 1 {
		t.Errorf("tier reserved = %d after a failed 4-seat basket, want 1", stock.Reserved)
	}
}

// The price hole: a premium chair may not be bought on a cheap tier's line.
//
// Without ticket_id in the claim's WHERE clause this is how somebody pays
// R$ 20,00 for the front row.
func TestASeatCannotBeClaimedThroughAnotherTier(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	ctx := context.Background()

	cheap := seedSecondTier(t, h.harness, 1_000)
	_, err := h.service.Start(ctx, StartInput{
		Items:          []orderdomain.DraftItem{{TicketID: cheap, SeatIDs: []string{h.seatIDs[0]}}},
		Confirm:        true,
		BuyerID:        h.ownerID,
		BuyerName:      "Maria Souza",
		BuyerEmail:     "maria@exemplo.com.br",
		BuyerDocument:  "12345678909",
		IdempotencyKey: testsupport.Unique("substitute"),
	})
	if err == nil {
		t.Fatal("a seat priced by one tier was claimed through another; the price is substitutable")
	}
	var unavailable *SeatsUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want the claim to refuse the seat", err)
	}
	if status := h.statusOf(t, h.seatIDs[0]); status != seatingdomain.StatusAvailable {
		t.Errorf("seat is %q after a refused cross-tier claim, want available", status)
	}
}

// One row per chair, each carrying its own label, and the money unchanged.
func TestASeatedOrderIsOneLinePerChair(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	wanted := []string{h.seatIDs[0], h.seatIDs[1], h.seatIDs[2]}

	order, err := h.buy(wanted, testsupport.Unique("lines"))
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if len(order.Items) != 3 {
		t.Fatalf("a 3-seat order has %d lines, want 3", len(order.Items))
	}
	seen := make(map[string]bool, 3)
	for _, line := range order.Items {
		if line.Quantity != 1 {
			t.Errorf("seated line %s has quantity %d, want 1", line.SeatID, line.Quantity)
		}
		if !line.Seated() {
			t.Errorf("line %s does not report itself as seated", line.ID)
		}
		if line.Seat.Row == "" || line.Seat.Seat == "" {
			t.Errorf("line %s carries no seat label", line.ID)
		}
		if line.Seat.Section != "Plateia A" {
			t.Errorf("line %s names section %q, want Plateia A", line.ID, line.Seat.Section)
		}
		seen[line.SeatID] = true
	}
	if len(seen) != 3 {
		t.Errorf("the order names %d distinct seats, want 3", len(seen))
	}
	// Three chairs at the tier's face value, charged exactly as three counted
	// tickets would be. One line of three and three lines of one must cost the
	// same money, or the fee is being taken on the line total.
	if order.SubtotalCents != 3*h.priceCents {
		t.Errorf("subtotal = %d, want %d", order.SubtotalCents, 3*h.priceCents)
	}
}

// A general-admission order still works, and touches no seat.
//
// This is the test that protects the party. The counted path must be exactly
// what it was, and a seated event living in the same tables must not change it.
func TestCountedOrdersAreUnaffectedBySeating(t *testing.T) {
	h := newHarness(t, 10)

	order, err := h.start(2, testsupport.Unique("counted"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(order.Items) != 1 {
		t.Fatalf("a counted order has %d lines, want 1", len(order.Items))
	}
	if order.Items[0].Quantity != 2 {
		t.Errorf("counted line quantity = %d, want 2", order.Items[0].Quantity)
	}
	if order.Items[0].Seated() {
		t.Error("a counted line reports itself as seated")
	}
	var seats int64
	if err := h.db.Raw("SELECT COUNT(*) FROM event_seats WHERE order_id = ?", order.ID).
		Scan(&seats).Error; err != nil {
		t.Fatalf("count seats: %v", err)
	}
	if seats != 0 {
		t.Errorf("a counted order holds %d seats, want 0", seats)
	}
}

// Releasing a hold gives the exact chairs back, and only the held ones.
func TestCancellingASeatedOrderReleasesItsChairs(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	ctx := context.Background()
	wanted := []string{h.seatIDs[4], h.seatIDs[5]}

	order, err := h.buy(wanted, testsupport.Unique("cancel"))
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	for _, id := range wanted {
		if status := h.statusOf(t, id); status != seatingdomain.StatusHeld {
			t.Fatalf("seat %s is %q after buying, want held", h.labels[id], status)
		}
	}

	if _, err := h.service.Cancel(ctx, order.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	for _, id := range wanted {
		if status := h.statusOf(t, id); status != seatingdomain.StatusAvailable {
			t.Errorf("seat %s is %q after cancelling, want available", h.labels[id], status)
		}
	}
	stock := h.stock(t)
	if stock.Reserved != 0 {
		t.Errorf("tier reserved = %d after cancelling, want 0", stock.Reserved)
	}
}

// The projection must not drift. The tier counter and the seats are two views
// of one truth, and a system where they disagree either oversells or withholds.
func TestTheTierCounterAgreesWithTheSeats(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	ctx := context.Background()

	if _, err := h.buy([]string{h.seatIDs[0], h.seatIDs[1]}, testsupport.Unique("agree")); err != nil {
		t.Fatalf("buy: %v", err)
	}

	counts, err := seatingRepository.NewSeatRepository(h.db).CountsByEvent(ctx, h.eventID)
	if err != nil {
		t.Fatalf("CountsByEvent: %v", err)
	}
	if len(counts) != 1 {
		t.Fatalf("counts cover %d tiers, want 1", len(counts))
	}
	stock := h.stock(t)
	tally := counts[0]
	if tally.Occupied() != stock.Sold+stock.Reserved {
		t.Errorf("seats say %d occupied, the tier says %d sold + %d reserved",
			tally.Occupied(), stock.Sold, stock.Reserved)
	}
	if tally.Total() != stock.Quantity {
		t.Errorf("seats total %d, the tier's quantity is %d", tally.Total(), stock.Quantity)
	}
}

// A blocked chair is not for sale, and blocking cannot take one somebody holds.
func TestBlockingWithholdsAChairAndNeverTakesASoldOne(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	ctx := context.Background()
	seats := seatingRepository.NewSeatRepository(h.db)

	held := h.seatIDs[7]
	if _, err := h.buy([]string{held}, testsupport.Unique("block")); err != nil {
		t.Fatalf("buy: %v", err)
	}

	broken := h.seatIDs[8]
	moved, err := seats.Block(ctx, h.eventID, []string{broken, held}, seatingdomain.BlockBroken)
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if moved != 1 {
		t.Fatalf("blocked %d seats, want 1: the held one must be refused", moved)
	}
	if status := h.statusOf(t, held); status != seatingdomain.StatusHeld {
		t.Errorf("a held seat became %q when blocked; somebody's ticket was cancelled", status)
	}

	_, err = h.buy([]string{broken}, testsupport.Unique("blocked-buy"))
	var unavailable *SeatsUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("buying a blocked seat returned %v, want it refused", err)
	}
}

// The map cursor only ever moves forward, which is what makes delta polling
// safe: a client that has seen version N can never miss a change.
func TestSeatChangesAdvanceTheMapCursor(t *testing.T) {
	h := newSeatedHarness(t, 4, 6)
	ctx := context.Background()
	seats := seatingRepository.NewSeatRepository(h.db)

	before, err := seats.ListByEvent(ctx, h.eventID, 0)
	if err != nil {
		t.Fatalf("ListByEvent: %v", err)
	}
	highest := int64(0)
	for _, seat := range before {
		if seat.Version > highest {
			highest = seat.Version
		}
	}

	if _, err := h.buy([]string{h.seatIDs[3]}, testsupport.Unique("cursor")); err != nil {
		t.Fatalf("buy: %v", err)
	}

	delta, err := seats.ListByEvent(ctx, h.eventID, highest)
	if err != nil {
		t.Fatalf("ListByEvent(since): %v", err)
	}
	if len(delta) != 1 {
		t.Fatalf("the delta carries %d seats, want exactly the one that changed", len(delta))
	}
	if delta[0].ID != h.seatIDs[3] {
		t.Errorf("the delta names seat %s, want %s", delta[0].ID, h.seatIDs[3])
	}
	if delta[0].Version <= highest {
		t.Errorf("version did not advance: %d is not above %d", delta[0].Version, highest)
	}
}

// A seat map cannot be laid out twice.
func TestMaterialisingTwiceIsRefused(t *testing.T) {
	h := newSeatedHarness(t, 2, 3)
	ctx := context.Background()

	_, err := seatingRepository.NewSeatRepository(h.db).Materialise(ctx, seatingdomain.MaterialisePlan{
		EventID:          h.eventID,
		LayoutID:         h.layoutID,
		TicketByCategory: map[string]string{testsupport.SeatedRoomCategory: h.ticketID},
	})
	if !errors.Is(err, seatingdomain.ErrAlreadyMaterialised) {
		t.Fatalf("second Materialise error = %v, want ErrAlreadyMaterialised", err)
	}
}

// seedSecondTier adds another tier to the harness event, for the cross-tier test.
func seedSecondTier(t *testing.T, h *harness, priceCents int64) string {
	t.Helper()
	id := testsupport.Unique("tkt")
	err := h.db.Exec(`
		INSERT INTO tickets
			(id, owner_id, event_id, title, description, price_cents, currency,
			 quantity, sold, reserved, status, created_at, updated_at)
		VALUES (?, ?, ?, 'Balcão', '', ?, 'BRL', 50, 0, 0, 'on_sale', NOW(), NOW())`,
		id, h.ownerID, h.eventID, priceCents).Error
	if err != nil {
		t.Fatalf("seed second tier: %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM order_items WHERE ticket_id = ?", id)
		h.db.Exec("DELETE FROM tickets WHERE id = ?", id)
	})
	return id
}

// Mixed orders hold every kind of admission together and release them together.
func TestMixedOrderReservesAndCancelsSeatsAndCountedTickets(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	ctx := context.Background()
	pista := seedSecondTier(t, h.harness, 12000)
	camarote := seedSecondTier(t, h.harness, 60000)
	placed, err := h.service.Start(ctx, StartInput{
		Items: []orderdomain.DraftItem{
			{TicketID: h.ticketID, SeatIDs: []string{h.seatIDs[0]}},
			{TicketID: pista, Quantity: 2},
			{TicketID: camarote, Quantity: 1},
		},
		BuyerID:        h.ownerID,
		IdempotencyKey: testsupport.Unique("mixed"),
	})
	if err != nil {
		t.Fatalf("mixed checkout: %v", err)
	}
	if placed.SubtotalCents != h.priceCents+2*12000+60000 {
		t.Fatalf("subtotal = %d", placed.SubtotalCents)
	}
	if len(placed.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(placed.Items))
	}
	if h.statusOf(t, h.seatIDs[0]) != seatingdomain.StatusHeld {
		t.Fatal("chair was not held")
	}
	for id, want := range map[string]int{h.ticketID: 1, pista: 2, camarote: 1} {
		stock, err := h.tickets.GetByID(ctx, id)
		if err != nil || stock.Reserved != want {
			t.Fatalf("reserved %s: %+v, %v", id, stock, err)
		}
	}
	if _, err := h.service.Cancel(ctx, placed.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if h.statusOf(t, h.seatIDs[0]) != seatingdomain.StatusAvailable {
		t.Fatal("chair was not released")
	}
	for _, id := range []string{h.ticketID, pista, camarote} {
		stock, err := h.tickets.GetByID(ctx, id)
		if err != nil || stock.Reserved != 0 {
			t.Fatalf("not released %s: %+v, %v", id, stock, err)
		}
	}
}

func TestMixedOrderRollsBackCountedStockWhenChairIsTaken(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	ctx := context.Background()
	pista := seedSecondTier(t, h.harness, 12000)
	if _, err := h.buy([]string{h.seatIDs[0]}, testsupport.Unique("taken")); err != nil {
		t.Fatal(err)
	}
	_, err := h.service.Start(ctx, StartInput{
		Items: []orderdomain.DraftItem{
			{TicketID: pista, Quantity: 2},
			{TicketID: h.ticketID, SeatIDs: []string{h.seatIDs[0]}},
		},
		BuyerID:        h.ownerID,
		IdempotencyKey: testsupport.Unique("mixed-lost"),
	})
	if !errors.Is(err, seatingdomain.ErrSeatsUnavailable) {
		t.Fatalf("error = %v", err)
	}
	stock, err := h.tickets.GetByID(ctx, pista)
	if err != nil || stock.Reserved != 0 {
		t.Fatalf("partial reservation: %+v, %v", stock, err)
	}
}

func TestNumberedTierCannotBeBoughtWithoutASeat(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	_, err := h.start(1, testsupport.Unique("missing-seat"))
	if !errors.Is(err, orderdomain.ErrInvalidTicket) {
		t.Fatalf("error = %v, want invalid ticket", err)
	}
	if stock := h.stock(t); stock.Reserved != 0 {
		t.Fatalf("reserved = %d", stock.Reserved)
	}
}

func TestMixedEventAllowsCountedOnlyPurchase(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	pista := seedSecondTier(t, h.harness, 12000)
	placed, err := h.service.Start(context.Background(), StartInput{
		Items:   []orderdomain.DraftItem{{TicketID: pista, Quantity: 2}},
		BuyerID: h.ownerID, IdempotencyKey: testsupport.Unique("mixed-counted-only"),
	})
	if err != nil {
		t.Fatalf("counted-only checkout in a seated event: %v", err)
	}
	// This order has no line on the harness's primary tier, so clean it up explicitly.
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM jobs WHERE payload->>'orderId' = ?", placed.ID)
		h.db.Exec("DELETE FROM orders WHERE id = ?", placed.ID)
	})
	if len(placed.Items) != 1 || placed.Items[0].Quantity != 2 || placed.Items[0].Seated() {
		t.Fatalf("unexpected lines: %+v", placed.Items)
	}
	for _, id := range h.seatIDs {
		if h.statusOf(t, id) != seatingdomain.StatusAvailable {
			t.Fatal("counted-only purchase claimed a chair")
		}
	}
}
