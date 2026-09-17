package payment

import (
	"context"
	"testing"

	admissionRepository "vozkot/infra/repositories/admission"
	"vozkot/infra/testsupport"
)

// The orders that paid before the door existed.
//
// Admissions are minted inside the transaction that crosses an order into
// paid, which is correct and is what stops a buyer ever holding a receipt with
// no way through the door. It also means an order that reached paid BEFORE
// this feature shipped has no tickets and never will, and its buyer opens
// their wallet to "no tickets issued yet" for good. On the development box
// that was all nine paid orders; on a live box office it is every order taken
// before the deploy.
//
// The backfill is the answer, and what makes it safe to run at all is that it
// reuses settlement's own issuing path rather than a second copy of it.

func TestBackfillIssuesAdmissionsForAnOrderThatMissedThem(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 20)
	ctx := context.Background()
	admissions := admissionRepository.NewAdmissionRepository(h.db)

	item := h.paid(t, 2)
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID) })

	// Exactly the state a pre-feature order is in: paid, with no tickets.
	if err := h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID).Error; err != nil {
		t.Fatalf("clear admissions: %v", err)
	}
	before, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("setup left %d admissions on the order", len(before))
	}

	if _, err := h.service.BackfillAdmissions(ctx, 100); err != nil {
		t.Fatalf("BackfillAdmissions() error = %v", err)
	}

	after, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder() after backfill: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("after the backfill the order for 2 tickets has %d admissions", len(after))
	}
	for _, admission := range after {
		if admission.Code.String() == "" {
			t.Errorf("admission %s was backfilled without a code", admission.ID)
		}
		if admission.EventID != h.eventID {
			t.Errorf("admission %s names event %q, want %q", admission.ID, admission.EventID, h.eventID)
		}
	}
}

// Running it twice must not double anybody's tickets.
//
// A backfill is the kind of thing an operator runs, loses the output of, and
// runs again. It is also the kind of thing that gets wired into a deploy step
// and therefore runs on every release. Both have to be free.
func TestBackfillIsIdempotent(t *testing.T) {
	testsupport.Encryption(t)
	h := newHarness(t, 20)
	ctx := context.Background()
	admissions := admissionRepository.NewAdmissionRepository(h.db)

	item := h.paid(t, 3)
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", item.ID) })

	// This order already has its tickets, from settlement. A backfill that
	// looked only at "is it paid" would mint a second set.
	issued, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(issued) != 3 {
		t.Fatalf("setup: paid order has %d admissions, want 3", len(issued))
	}

	for round := 1; round <= 2; round++ {
		if _, err := h.service.BackfillAdmissions(ctx, 100); err != nil {
			t.Fatalf("BackfillAdmissions() round %d error = %v", round, err)
		}
	}

	after, err := admissions.ListByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListByOrder() after backfill: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("two backfill runs left %d admissions on a 3-ticket order", len(after))
	}

	// And the codes are the same ones. A backfill that reissued would hand a
	// buyer a new code while the email they already have carries the old one.
	held := map[string]bool{}
	for _, admission := range issued {
		held[admission.ID] = true
	}
	for _, admission := range after {
		if !held[admission.ID] {
			t.Errorf("admission %s appeared after a backfill of an order that was already issued", admission.ID)
		}
	}
}
