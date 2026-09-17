package payment

import (
	"context"
	"testing"
	"time"

	orderdomain "vozkot/domain/order"
	seatingdomain "vozkot/domain/seating"
	"vozkot/infra/mercadopago"
	admissionRepository "vozkot/infra/repositories/admission"
	seatingRepository "vozkot/infra/repositories/seating"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	"vozkot/usecases/checkout"
	queueUsecase "vozkot/usecases/queue"
)

// seatedBuyer is the real checkout service on the harness's database.
//
// The property under test spans two use cases — a claim in checkout, a commit
// in settlement — and a hand-written order row would skip the claim that makes
// it true. So the order is placed the way a buyer places it.
func seatedBuyer(h *harness) *checkout.Service {
	return checkout.NewService(
		uow.NewRunner(h.db), h.orders, h.tickets, nil,
		queueUsecase.NewDispatcher(nil),
		checkout.Settings{HoldFor: 30 * time.Minute, CartHoldFor: 10 * time.Minute},
	)
}

// The other half of reserved seating: what settlement does with a held chair.
//
// usecases/checkout proves a chair cannot be claimed twice. This proves that
// paying for it turns it from held into SOLD, mints exactly one admission per
// chair, and that the admission carries the seat — which is the whole reason a
// door can tell somebody where to sit.

// seatFor lays out a room for the payment harness's event and returns the
// event seats, in row-then-seat order.
func seatFor(t *testing.T, h *harness, rows, perRow int) []seatingdomain.EventSeat {
	t.Helper()
	ctx := context.Background()

	room := testsupport.SeedSeatedRoom(t, h.db, h.ownerID, rows, perRow)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)

	seats := seatingRepository.NewSeatRepository(h.db)
	if _, err := seats.Materialise(ctx, seatingdomain.MaterialisePlan{
		EventID:          h.eventID,
		LayoutID:         room.LayoutID,
		TicketByCategory: map[string]string{room.Category: h.ticketID},
	}); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	live, err := seats.ListByEvent(ctx, h.eventID, 0)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	if len(live) != rows*perRow {
		t.Fatalf("materialised %d seats, want %d", len(live), rows*perRow)
	}
	return live
}

func seatStatus(t *testing.T, h *harness, seatID string) seatingdomain.Status {
	t.Helper()
	var status string
	if err := h.db.Raw("SELECT status FROM event_seats WHERE id = ?", seatID).
		Scan(&status).Error; err != nil {
		t.Fatalf("read seat status: %v", err)
	}
	return seatingdomain.Status(status)
}

// Paying for chairs sells them, and mints one ticket per chair that names it.
func TestPayingForSeatsSellsThemAndNamesThemOnTheAdmission(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 24)
	ctx := context.Background()

	room := seatFor(t, h, 4, 6)
	wanted := []string{room[0].ID, room[1].ID}

	// Bought through the real checkout service, on the same database, because
	// the property under test spans both use cases and a hand-written order row
	// would skip the claim that makes it true.
	buyer := seatedBuyer(h)
	order, err := buyer.Start(ctx, checkout.StartInput{
		Items:          []orderdomain.DraftItem{{TicketID: h.ticketID, SeatIDs: wanted}},
		Confirm:        true,
		BuyerID:        h.ownerID,
		BuyerName:      "Maria Souza",
		BuyerEmail:     "maria@exemplo.com.br",
		BuyerDocument:  "12345678909",
		IdempotencyKey: testsupport.Unique("seated"),
	})
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM admissions WHERE order_id = ?", order.ID)
	})

	for _, id := range wanted {
		if status := seatStatus(t, h, id); status != seatingdomain.StatusHeld {
			t.Fatalf("seat is %q before payment, want held", status)
		}
	}

	paymentID := h.charge(t, order)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncOrder(ctx, order.ID); err != nil {
		t.Fatalf("SyncOrder(): %v", err)
	}

	stored := h.order(t, order.ID)
	if stored.Status != orderdomain.StatusPaid {
		t.Fatalf("order is %q, want paid", stored.Status)
	}
	// The chairs moved with the money, in the same transaction. Left held, the
	// lapsed-hold sweep would put them back on sale minutes later and sell them
	// to somebody else while this buyer holds a valid ticket.
	for _, id := range wanted {
		if status := seatStatus(t, h, id); status != seatingdomain.StatusSold {
			t.Errorf("seat is %q after payment, want sold", status)
		}
	}

	admissions, err := admissionRepository.NewAdmissionRepository(h.db).ListByOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(admissions) != 2 {
		t.Fatalf("a 2-seat order issued %d admissions, want 2", len(admissions))
	}
	seen := make(map[string]bool, 2)
	for _, admission := range admissions {
		if admission.SeatID == "" {
			t.Errorf("admission %s carries no seat; the door cannot say where to sit", admission.ID)
		}
		if admission.Seat.Row == "" || admission.Seat.Seat == "" {
			t.Errorf("admission %s has no seat label: %#v", admission.ID, admission.Seat)
		}
		if admission.Seat.Section != "Plateia A" {
			t.Errorf("admission %s names section %q", admission.ID, admission.Seat.Section)
		}
		if seen[admission.SeatID] {
			t.Errorf("two admissions share seat %s", admission.SeatID)
		}
		seen[admission.SeatID] = true
	}
	for _, id := range wanted {
		if !seen[id] {
			t.Errorf("no admission was issued for seat %s", id)
		}
	}
}

// A refund puts the chair back on sale.
func TestRefundingASeatedOrderReturnsTheChair(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 24)
	ctx := context.Background()

	room := seatFor(t, h, 4, 6)
	seat := room[3].ID

	buyer := seatedBuyer(h)
	order, err := buyer.Start(ctx, checkout.StartInput{
		Items:          []orderdomain.DraftItem{{TicketID: h.ticketID, SeatIDs: []string{seat}}},
		Confirm:        true,
		BuyerID:        h.ownerID,
		BuyerName:      "Maria Souza",
		BuyerEmail:     "maria@exemplo.com.br",
		BuyerDocument:  "12345678909",
		IdempotencyKey: testsupport.Unique("refund-seat"),
	})
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM admissions WHERE order_id = ?", order.ID)
	})

	paymentID := h.charge(t, order)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncOrder(ctx, order.ID); err != nil {
		t.Fatalf("SyncOrder(): %v", err)
	}
	if status := seatStatus(t, h, seat); status != seatingdomain.StatusSold {
		t.Fatalf("seat is %q after payment, want sold", status)
	}

	h.provider.move(paymentID, mercadopago.StatusRefunded, "")
	if err := h.service.SyncOrder(ctx, order.ID); err != nil {
		t.Fatalf("SyncOrder() after refund: %v", err)
	}

	if stored := h.order(t, order.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("order is %q, want refunded", stored.Status)
	}
	// The money went back, so the chair goes with it. A refunded seat left
	// sold is a chair nobody can ever buy again.
	if status := seatStatus(t, h, seat); status != seatingdomain.StatusAvailable {
		t.Errorf("seat is %q after a refund, want available", status)
	}
}
