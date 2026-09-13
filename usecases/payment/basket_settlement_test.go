package payment

import (
	"context"
	"testing"
	"time"

	orderdomain "vozkot/domain/order"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/mercadopago"
	"vozkot/infra/testsupport"
)

// Settling an order that covers several tiers of one night.
//
// The single-tier settlement tests next door already cover the state machine.
// What is new here is that every stock movement is now a LOOP, and a loop has
// two failure modes a single call does not: stopping halfway, and running in a
// different order than some other transaction. Both cost money, so both are
// tested against the real database rather than reasoned about.

// secondTier adds another tier to the harness's event and returns its id.
func (h *harness) secondTier(t *testing.T, title string, price int64, stock int) string {
	t.Helper()
	item, err := ticketdomain.New(testsupport.Unique("tkt"), h.ownerID, ticketdomain.Draft{
		EventID:    h.eventID,
		Title:      title,
		PriceCents: price,
		Quantity:   stock,
		Status:     ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build %s: %v", title, err)
	}
	if err := h.tickets.Create(context.Background(), item); err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM order_items WHERE ticket_id = ?", item.ID)
		h.db.Exec("DELETE FROM tickets WHERE id = ?", item.ID)
	})
	return item.ID
}

// basketOrder reserves both tiers and opens one pending order over them, the
// way checkout does.
func (h *harness) basketOrder(t *testing.T, secondTierID string, first, second int) *orderdomain.Order {
	t.Helper()
	ctx := context.Background()

	for _, held := range []struct {
		id       string
		quantity int
	}{{h.ticketID, first}, {secondTierID, second}} {
		reserved, err := h.tickets.Reserve(ctx, held.id, held.quantity)
		if err != nil || !reserved {
			t.Fatalf("reserve %s: reserved=%t err=%v", held.id, reserved, err)
		}
	}

	item, err := orderdomain.New(testsupport.Unique("ord"), orderdomain.Draft{
		EventID:       h.eventID,
		BuyerID:       h.ownerID,
		BuyerName:     "Maria Souza",
		BuyerEmail:    "maria@exemplo.com.br",
		BuyerDocument: "12345678909",
		Items: []orderdomain.Item{
			{ID: testsupport.Unique("oi"), TicketID: h.ticketID, TicketTitle: "Pista", Quantity: first, UnitPriceCents: 24000},
			{ID: testsupport.Unique("oi"), TicketID: secondTierID, TicketTitle: "Camarote", Quantity: second, UnitPriceCents: 50000},
		},
	}, 30*time.Minute, time.Now())
	if err != nil {
		t.Fatalf("build order: %v", err)
	}
	if err := h.orders.Create(ctx, item); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return item
}

func (h *harness) tierStock(t *testing.T, ticketID string) ticketdomain.Ticket {
	t.Helper()
	item, err := h.tickets.GetByID(context.Background(), ticketID)
	if err != nil {
		t.Fatalf("read tier %s: %v", ticketID, err)
	}
	return *item
}

// TestApprovedBasketSellsEveryTier: the money arrived for three tickets across
// two tiers, so both tiers must move from held to sold.
func TestApprovedBasketSellsEveryTier(t *testing.T) {
	h := newHarness(t, 10)
	camarote := h.secondTier(t, "Camarote", 50000, 10)
	item := h.basketOrder(t, camarote, 2, 1)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)

	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Fatalf("status = %q, want paid", stored.Status)
	}
	if pista := h.stock(t); pista.Sold != 2 || pista.Reserved != 0 {
		t.Fatalf("pista sold = %d reserved = %d, want 2 and 0", pista.Sold, pista.Reserved)
	}
	if box := h.tierStock(t, camarote); box.Sold != 1 || box.Reserved != 0 {
		t.Fatalf("camarote sold = %d reserved = %d, want 1 and 0: the second line must settle too", box.Sold, box.Reserved)
	}
}

// TestChargedBasketIsPricedAcrossEveryLine.
//
// The provider is asked for one amount covering the whole basket. Charging only
// the first line would take 2 x 24000 for an order worth 2 x 24000 + 1 x 50000,
// and the buyer would be admitted on tickets nobody paid for.
func TestChargedBasketIsPricedAcrossEveryLine(t *testing.T) {
	h := newHarness(t, 10)
	camarote := h.secondTier(t, "Camarote", 50000, 10)
	item := h.basketOrder(t, camarote, 2, 1)

	if item.TotalCents != 98000 {
		t.Fatalf("total = %d, want 98000 (2 x 24000 + 1 x 50000)", item.TotalCents)
	}
	paymentID := h.charge(t, item)

	charged := h.provider.amount(paymentID)
	if charged != 980.0 {
		t.Fatalf("charged %.2f, want 980.00 — the whole basket, not one line", charged)
	}
}

// TestRefundedBasketReturnsEveryTierToStock.
func TestRefundedBasketReturnsEveryTierToStock(t *testing.T) {
	h := newHarness(t, 10)
	camarote := h.secondTier(t, "Camarote", 50000, 10)
	item := h.basketOrder(t, camarote, 2, 1)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("settle: %v", err)
	}

	h.provider.move(paymentID, mercadopago.StatusRefunded, "")
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("refund sync: %v", err)
	}

	if pista := h.stock(t); pista.Sold != 0 {
		t.Fatalf("pista sold = %d, want 0", pista.Sold)
	}
	if box := h.tierStock(t, camarote); box.Sold != 0 {
		t.Fatalf("camarote sold = %d, want 0: every line comes back", box.Sold)
	}
}

// TestLatePaymentOnABasketIsAllOrNothing is the case a per-line re-reservation
// would settle into an event that is oversold on one tier and half-admitted on
// the other.
//
// The hold lapsed, Pista is still available, and Camarote sold out while the
// buyer was in their banking app. Taking the Pista back and calling the order
// paid would admit them on two of three tickets they paid in full for — and
// would take stock for an order that cannot be honoured. The only honest answer
// is that the box office owes a refund, and that the Pista it briefly took back
// is released again.
func TestLatePaymentOnABasketIsAllOrNothing(t *testing.T) {
	h := newHarness(t, 10)
	ctx := context.Background()
	camarote := h.secondTier(t, "Camarote", 50000, 1)
	item := h.basketOrder(t, camarote, 2, 1)
	paymentID := h.charge(t, item)
	h.expire(t, item)

	// Someone else takes the only Camarote while this buyer is away.
	reserved, err := h.tickets.Reserve(ctx, camarote, 1)
	if err != nil || !reserved {
		t.Fatalf("second buyer could not reserve: %t %v", reserved, err)
	}
	if err := h.tickets.Commit(ctx, camarote, 1); err != nil {
		t.Fatalf("second buyer could not buy: %v", err)
	}

	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	stored := h.order(t, item.ID)
	if stored.Status != orderdomain.StatusRefundRequired {
		t.Fatalf("status = %q, want %q", stored.Status, orderdomain.StatusRefundRequired)
	}
	// The Pista that was briefly taken back must have been let go again.
	pista := h.stock(t)
	if pista.Reserved != 0 || pista.Sold != 0 {
		t.Fatalf("pista reserved = %d sold = %d, want 0 and 0: the partial hold must be undone",
			pista.Reserved, pista.Sold)
	}
	if pista.Available() != 10 {
		t.Fatalf("pista available = %d, want 10 back on sale", pista.Available())
	}
	box := h.tierStock(t, camarote)
	if box.Sold+box.Reserved > box.Quantity {
		t.Fatalf("camarote oversold: sold %d + reserved %d > %d", box.Sold, box.Reserved, box.Quantity)
	}
}

// TestExpiredBasketReturnsEveryTier: the sweep releases every line, not the
// first one.
func TestExpiredBasketReturnsEveryTier(t *testing.T) {
	h := newHarness(t, 10)
	camarote := h.secondTier(t, "Camarote", 50000, 10)
	item := h.basketOrder(t, camarote, 2, 3)
	paymentID := h.charge(t, item)

	h.provider.move(paymentID, mercadopago.StatusCancelled, "")
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if pista := h.stock(t); pista.Reserved != 0 {
		t.Fatalf("pista reserved = %d, want 0", pista.Reserved)
	}
	if box := h.tierStock(t, camarote); box.Reserved != 0 {
		t.Fatalf("camarote reserved = %d, want 0", box.Reserved)
	}
}
