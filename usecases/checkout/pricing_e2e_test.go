package checkout

import (
	"context"
	"testing"
	"time"

	orderdomain "vozkot/domain/order"
	"vozkot/domain/pricing"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/testsupport"
)

// The service fee, end to end, against a real PostgreSQL.
//
// The arithmetic itself is covered by domain/pricing's unit tests. What these
// cannot cover is the part that actually decides what a buyer is charged: that
// the rate reaches the order, that the split survives a write and a read, that
// the two phases of checkout agree on it, and that the organiser's share is
// still the number they typed. Every one of those is a boundary between two
// pieces of code, and a fee that is right in the domain and lost at one of them
// is indistinguishable, from the buyer's side, from a fee that was never
// computed.
//
// Real Postgres rather than a fake repository, for the same reason the
// inventory tests use one: the three money columns are written by GORM and read
// back through a mapper, and a fake would be asserting that the test's own
// struct literal round-trips.

// addTier puts a second tier on the harness's event.
//
// Used by the rounding case, which needs a face value whose ten per cent is not
// a whole centavo. It reuses the harness's owner and event so the cleanup that
// already removes them covers this too.
func (h *harness) addTier(t *testing.T, priceCents int64, capacity int) string {
	t.Helper()
	ctx := context.Background()
	item, err := ticketdomain.New(testsupport.Unique("tkt"), h.ownerID, ticketdomain.Draft{
		EventID:    h.eventID,
		Title:      "Mezanino",
		PriceCents: priceCents,
		Quantity:   capacity,
		Status:     ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build tier at %d: %v", priceCents, err)
	}
	if err := h.tickets.Create(ctx, item); err != nil {
		t.Fatalf("create tier at %d: %v", priceCents, err)
	}
	t.Cleanup(func() {
		// Parents first, and NEVER order_items on their own.
		//
		// order_items cascades from orders, so deleting the order takes its
		// lines with it. An earlier version of this deleted the lines directly
		// by ticket_id, which left the ORDER standing with no lines at all —
		// and an order with a total and no items is one the charge path logs
		// as "(0 items)" and the admission issuer silently issues nothing for.
		// The orphans outlived the test and turned up in a running server's
		// log, which is how this was found.
		h.db.Exec(`DELETE FROM jobs WHERE payload->>'orderId' IN
			(SELECT order_id FROM order_items WHERE ticket_id = ?)`, item.ID)
		h.db.Exec(`DELETE FROM admissions WHERE ticket_id = ?`, item.ID)
		h.db.Exec(`DELETE FROM orders WHERE id IN
			(SELECT order_id FROM order_items WHERE ticket_id = ?)`, item.ID)
		h.db.Exec("DELETE FROM tickets WHERE id = ?", item.ID)
	})
	return item.ID
}

// assertSplit is the same set of claims made about an order wherever it came
// from: the service's return value, or the same row read back out of Postgres.
//
// One helper rather than the assertions inline twice, because the point of
// reading the order back is to compare it against the SAME expectations, and
// two copies that drifted would hide exactly the bug this file is about.
func assertSplit(t *testing.T, where string, item *orderdomain.Order, face, fee int64) {
	t.Helper()
	if item.SubtotalCents != face {
		t.Errorf("%s: subtotal = %d, want %d (the organiser's share)", where, item.SubtotalCents, face)
	}
	if item.BuyerFeeCents != fee {
		t.Errorf("%s: service fee = %d, want %d", where, item.BuyerFeeCents, fee)
	}
	if item.TotalCents != face+fee {
		t.Errorf("%s: total = %d, want %d (subtotal %d + fee %d)",
			where, item.TotalCents, face+fee, face, fee)
	}
	if err := item.ValidatePricing(); err != nil {
		t.Errorf("%s: %v", where, err)
	}
}

// The whole feature in one test: the organiser prices a tier, the buyer pays
// that plus ten per cent, and the split is still intact after a round trip
// through the database.
func TestServiceFeeIsChargedOnTopAndSurvivesTheDatabase(t *testing.T) {
	fee, err := pricing.PlatformFee()
	if err != nil {
		t.Fatalf("PlatformFee(): %v", err)
	}
	h := newHarnessWithFee(t, 10, fee)
	ctx := context.Background()

	const quantity = 3
	wantFace := h.priceCents * quantity
	wantUnitFee := fee.On(h.priceCents)
	wantFee := wantUnitFee * quantity

	// Ten per cent of R$ 240,00 is R$ 24,00. Asserted rather than assumed: if
	// the in-code rate ever moves, this line is the one that explains why the
	// rest of the test changed.
	if wantUnitFee != 2400 {
		t.Fatalf("unit fee on R$ 240,00 = %d, want 2400; the platform rate is no longer ten per cent", wantUnitFee)
	}

	item, err := h.service.Start(ctx, StartInput{
		Items:      []orderdomain.DraftItem{{TicketID: h.ticketID, Quantity: quantity}},
		BuyerID:    h.ownerID,
		BuyerName:  "Maria Souza",
		BuyerEmail: "maria@exemplo.com.br",
	})
	if err != nil {
		t.Fatalf("start checkout: %v", err)
	}
	assertSplit(t, "reserved order", item, wantFace, wantFee)

	// The line carries the per-unit fee too, because that is the number the
	// event page quoted and a receipt has to be able to show it per ticket.
	if len(item.Items) != 1 {
		t.Fatalf("order has %d lines, want 1", len(item.Items))
	}
	line := item.Items[0]
	if line.UnitFeeCents != wantUnitFee || line.FeeCents != wantFee {
		t.Errorf("line fee = %d unit / %d total, want %d / %d",
			line.UnitFeeCents, line.FeeCents, wantUnitFee, wantFee)
	}
	if line.TotalCents != wantFace {
		t.Errorf("line face value = %d, want %d", line.TotalCents, wantFace)
	}
	if got := line.ChargedCents(); got != wantFace+wantFee {
		t.Errorf("line charged = %d, want %d", got, wantFace+wantFee)
	}

	// Confirming must not re-price. It extends the hold and takes the buyer's
	// details; an order that grew a second fee here would charge the buyer
	// twice over for the same commission.
	confirmed, err := h.service.Confirm(ctx, ConfirmInput{
		OrderID:       item.ID,
		BuyerID:       h.ownerID,
		BuyerName:     "Maria Souza",
		BuyerEmail:    "maria@exemplo.com.br",
		BuyerDocument: "12345678909",
	})
	if err != nil {
		t.Fatalf("confirm checkout: %v", err)
	}
	assertSplit(t, "confirmed order", confirmed, wantFace, wantFee)

	// And the row itself. This is the copy the charge job, the receipt and the
	// organiser's payout will each read; everything above was still in memory.
	stored, err := h.orders.GetByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("read order back: %v", err)
	}
	assertSplit(t, "order read from postgres", stored, wantFace, wantFee)

	// Read past the mapper as well, straight out of the three columns, so a
	// mapper that filled them from the wrong fields cannot pass.
	var row struct {
		SubtotalCents   int64
		ServiceFeeCents int64
		TotalCents      int64
	}
	if err := h.db.Raw(
		"SELECT subtotal_cents, service_fee_cents, total_cents FROM orders WHERE id = ?", item.ID,
	).Scan(&row).Error; err != nil {
		t.Fatalf("read money columns: %v", err)
	}
	if row.SubtotalCents != wantFace || row.ServiceFeeCents != wantFee || row.TotalCents != wantFace+wantFee {
		t.Errorf("orders row = subtotal %d, fee %d, total %d; want %d, %d, %d",
			row.SubtotalCents, row.ServiceFeeCents, row.TotalCents,
			wantFace, wantFee, wantFace+wantFee)
	}

	// The invariant the feature exists for, stated on its own: the organiser is
	// owed exactly what they priced, and the buyer paid more than that.
	if stored.SubtotalCents != h.priceCents*quantity {
		t.Errorf("the organiser's share moved: %d, want %d", stored.SubtotalCents, h.priceCents*quantity)
	}
	if stored.TotalCents <= stored.SubtotalCents {
		t.Errorf("the buyer was charged %d, which is not more than the face value %d",
			stored.TotalCents, stored.SubtotalCents)
	}
}

// The fee must be the only thing that adds anything.
//
// Without this, a bug that added a fee somewhere other than domain/pricing
// would be invisible: every assertion in the test above would still pass
// because it computes the expected numbers from the same fee.
func TestWithoutAFeeTheBuyerPaysTheTierPrice(t *testing.T) {
	h := newHarnessWithFee(t, 10, pricing.Fee{})
	ctx := context.Background()

	item, err := h.service.Start(ctx, StartInput{
		Items:      []orderdomain.DraftItem{{TicketID: h.ticketID, Quantity: 2}},
		BuyerID:    h.ownerID,
		BuyerName:  "Maria Souza",
		BuyerEmail: "maria@exemplo.com.br",
	})
	if err != nil {
		t.Fatalf("start checkout: %v", err)
	}

	assertSplit(t, "unfeed order", item, h.priceCents*2, 0)
	if item.TotalCents != h.priceCents*2 {
		t.Fatalf("total = %d with no fee, want the tier price %d", item.TotalCents, h.priceCents*2)
	}
}

// Rounding, through the whole stack rather than in the domain alone.
//
// R$ 33,35 is the case that matters: ten per cent is 333.5 centavos, which
// rounds to 334, and three tickets must therefore cost 3 × 334 and not the 1000
// that a fee taken on the R$ 100,05 line total would produce. The buyer can
// check this by hand from the per-ticket price the page showed them, so the
// centavo is not academic.
func TestFeeRoundingIsPerTicketAllTheWayToTheDatabase(t *testing.T) {
	fee, err := pricing.PlatformFee()
	if err != nil {
		t.Fatalf("PlatformFee(): %v", err)
	}
	h := newHarnessWithFee(t, 10, fee)
	ctx := context.Background()

	const oddPrice = 3_335
	const quantity = 3
	tierID := h.addTier(t, oddPrice, 10)

	unitFee := fee.On(oddPrice)
	if unitFee != 334 {
		t.Fatalf("unit fee on %d = %d, want 334 (half a centavo rounds up)", oddPrice, unitFee)
	}
	onLineTotal := fee.On(oddPrice * quantity)
	if unitFee*quantity == onLineTotal {
		t.Fatalf("this price no longer distinguishes per-unit from per-line rounding; pick another")
	}

	item, err := h.service.Start(ctx, StartInput{
		Items:      []orderdomain.DraftItem{{TicketID: tierID, Quantity: quantity}},
		BuyerID:    h.ownerID,
		BuyerName:  "Maria Souza",
		BuyerEmail: "maria@exemplo.com.br",
	})
	if err != nil {
		t.Fatalf("start checkout: %v", err)
	}

	wantFee := unitFee * quantity
	assertSplit(t, "odd-priced order", item, oddPrice*quantity, wantFee)

	stored, err := h.orders.GetByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("read order back: %v", err)
	}
	assertSplit(t, "odd-priced order from postgres", stored, oddPrice*quantity, wantFee)
	if stored.BuyerFeeCents == onLineTotal {
		t.Errorf("the stored fee %d is ten per cent of the LINE TOTAL; it must be the per-ticket fee times %d = %d",
			stored.BuyerFeeCents, quantity, wantFee)
	}
}

// A basket spanning two tiers at different prices.
//
// The order's fee has to be the sum of the lines' fees, not a rate applied to
// the order subtotal — those differ as soon as two prices round in opposite
// directions, and this is the shape that catches a total recomputed from the
// rate at the order level.
func TestFeeOnAMixedBasketIsTheSumOfItsLines(t *testing.T) {
	fee, err := pricing.PlatformFee()
	if err != nil {
		t.Fatalf("PlatformFee(): %v", err)
	}
	h := newHarnessWithFee(t, 10, fee)
	ctx := context.Background()

	const oddPrice = 3_335
	otherTier := h.addTier(t, oddPrice, 10)

	item, err := h.service.Start(ctx, StartInput{
		Items: []orderdomain.DraftItem{
			{TicketID: h.ticketID, Quantity: 2},
			{TicketID: otherTier, Quantity: 3},
		},
		BuyerID:    h.ownerID,
		BuyerName:  "Maria Souza",
		BuyerEmail: "maria@exemplo.com.br",
	})
	if err != nil {
		t.Fatalf("start checkout: %v", err)
	}

	wantFace := h.priceCents*2 + oddPrice*3
	wantFee := fee.On(h.priceCents)*2 + fee.On(oddPrice)*3
	assertSplit(t, "mixed basket", item, wantFace, wantFee)

	// The same claim from the other direction: whatever the lines say, the
	// order must agree with them. ValidatePricing checks this, so assert it
	// actually ran over two lines rather than skipping an empty set.
	if len(item.Items) != 2 {
		t.Fatalf("basket has %d lines, want 2", len(item.Items))
	}

	stored, err := h.orders.GetByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("read order back: %v", err)
	}
	assertSplit(t, "mixed basket from postgres", stored, wantFace, wantFee)
}
