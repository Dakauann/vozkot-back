package payment

import (
	"context"
	"testing"

	admissiondomain "vozkot/domain/admission"
	authdomain "vozkot/domain/auth"
	orderdomain "vozkot/domain/order"
	userdomain "vozkot/domain/user"
	admissionRepository "vozkot/infra/repositories/admission"
	"vozkot/infra/testsupport"
)

// The link the whole feature rests on: a PAID order has tickets, and it has
// them by the time the payment is recorded rather than shortly afterwards.
//
// This is the join between settlement and the door, and it is the one thing
// neither side's own tests can see. usecases/admission proves a code admits
// once; the admission repository proves issuing is idempotent; only this
// proves that paying is what causes the codes to exist at all.

func TestPayingAnOrderIssuesItsAdmissions(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 20)
	ctx := context.Background()
	admissions := admissionRepository.NewAdmissionRepository(h.db)

	item := h.paid(t, 3)
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID) })

	issued, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(issued) != 3 {
		t.Fatalf("a paid order for 3 tickets has %d admissions", len(issued))
	}

	codes := map[admissiondomain.Code]bool{}
	for _, admission := range issued {
		if admission.Status != admissiondomain.StatusIssued {
			t.Errorf("admission %s is %q, want issued", admission.ID, admission.Status)
		}
		if admission.OrderID != item.ID {
			t.Errorf("admission %s names order %q, want %q", admission.ID, admission.OrderID, item.ID)
		}
		if admission.EventID != h.eventID {
			t.Errorf("admission %s names event %q, want %q", admission.ID, admission.EventID, h.eventID)
		}
		if admission.TicketID != h.ticketID {
			t.Errorf("admission %s names tier %q, want %q", admission.ID, admission.TicketID, h.ticketID)
		}
		if _, err := admissiondomain.ParseCode(admission.Code.String()); err != nil {
			t.Errorf("admission %s has an unusable code: %v", admission.ID, err)
		}
		if codes[admission.Code] {
			t.Errorf("two of one order's admissions share the code %q", admission.Code)
		}
		codes[admission.Code] = true
	}
}

// An unpaid order has no tickets. Without this, "paying issues them" could be
// satisfied by issuing them at checkout.
func TestAnUnpaidOrderHasNoAdmissions(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 20)
	ctx := context.Background()
	admissions := admissionRepository.NewAdmissionRepository(h.db)

	item := h.pendingOrder(t, 2)
	h.charge(t, item)
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID) })

	issued, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(issued) != 0 {
		t.Fatalf("an order awaiting payment already has %d admissions", len(issued))
	}
}

// A redelivered approval must not mint a second set. The webhook path is the
// one that actually happens twice.
func TestSettlingTwiceIssuesOneSet(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 20)
	ctx := context.Background()
	admissions := admissionRepository.NewAdmissionRepository(h.db)

	item := h.paid(t, 2)
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID) })

	// The same approval again, which is what a provider redelivery is.
	if err := h.service.SyncOrder(ctx, item.ID); err != nil {
		t.Fatalf("SyncOrder(): %v", err)
	}
	if err := h.service.SyncOrder(ctx, item.ID); err != nil {
		t.Fatalf("the second SyncOrder(): %v", err)
	}

	issued, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(issued) != 2 {
		t.Fatalf("%d admissions after settling three times, want 2", len(issued))
	}
}

// Refunding withdraws entry, in the transaction that moves the money.
func TestRefundingAnOrderVoidsItsAdmissions(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 20)
	ctx := context.Background()
	admissions := admissionRepository.NewAdmissionRepository(h.db)

	item := h.paid(t, 2)
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID) })

	operator := authdomain.Actor{ID: "usr_ops", Role: userdomain.RoleAdmin}
	if _, err := h.service.RequestRefund(ctx, operator, item.ID); err != nil {
		t.Fatalf("RequestRefund(): %v", err)
	}
	// The refund is scheduled, not performed, so drive the settlement the way
	// the provider's confirmation would.
	if err := h.service.Refund(ctx, item.ID); err != nil {
		t.Fatalf("Refund(): %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("order status = %q, want refunded", stored.Status)
	}

	issued, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(issued) == 0 {
		t.Fatal("the refunded order has no admissions to check")
	}
	for _, admission := range issued {
		if admission.Status != admissiondomain.StatusVoid {
			t.Errorf("admission %s is %q after a refund, want void", admission.ID, admission.Status)
		}
	}
}
