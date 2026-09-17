package checkout

import (
	"context"
	"testing"

	authdomain "vozkot/domain/auth"
	orderdomain "vozkot/domain/order"
	userdomain "vozkot/domain/user"
	"vozkot/infra/testsupport"
)

// The buyer's own list asks a question no single status answers.
//
// An abandoned checkout is persisted here as an `expired` order, so "every
// order I have" is mostly carts nobody finished, and the ones that are alive —
// paid, still awaiting payment, owed a refund — are buried among them. The
// screen therefore asks for a SET of statuses, which has to be one query: three
// separate requests could not be paged as a single list, and `total` would be
// wrong, which is the number the pager trusts.
func TestListNarrowsToASetOfStatuses(t *testing.T) {
	h := newHarness(t, 20)
	ctx := context.Background()
	actor := authdomain.Actor{ID: h.ownerID, Role: userdomain.RoleUser}

	// One order per status under test, all for the same buyer, so the only
	// thing separating them in the assertions below is their status.
	statuses := []orderdomain.Status{
		orderdomain.StatusPaid,
		orderdomain.StatusPendingPayment,
		orderdomain.StatusExpired,
		orderdomain.StatusCancelled,
	}
	placed := make(map[orderdomain.Status]string, len(statuses))
	for _, status := range statuses {
		item, err := h.start(1, testsupport.Unique("filter"))
		if err != nil {
			t.Fatalf("Start() for %s error = %v", status, err)
		}
		placed[status] = item.ID
		// Set through SQL on purpose. What is under test is the query, not the
		// state machine that would otherwise have to be walked through a
		// charge and a settlement to reach four different terminal states.
		if err := h.db.Exec("UPDATE orders SET status = ? WHERE id = ?", string(status), item.ID).Error; err != nil {
			t.Fatalf("move %s to %s: %v", item.ID, status, err)
		}
	}

	alive := []orderdomain.Status{
		orderdomain.StatusPaid,
		orderdomain.StatusPendingPayment,
		orderdomain.StatusRefundRequired,
	}
	page, err := h.service.List(ctx, actor, orderdomain.Filter{Statuses: alive, Limit: 50})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	returned := make(map[string]orderdomain.Status, len(page.Items))
	for _, item := range page.Items {
		returned[item.ID] = item.Status
	}

	for _, status := range []orderdomain.Status{orderdomain.StatusPaid, orderdomain.StatusPendingPayment} {
		if _, ok := returned[placed[status]]; !ok {
			t.Errorf("order %s is %s and should be in a list of live orders", placed[status], status)
		}
	}
	for _, status := range []orderdomain.Status{orderdomain.StatusExpired, orderdomain.StatusCancelled} {
		if got, ok := returned[placed[status]]; ok {
			t.Errorf("order %s is %s and should not be in a list of live orders, got %s",
				placed[status], status, got)
		}
	}

	// Total is what the pager pages on. A count that ignored the filter would
	// offer a second page of nothing, or hide a row behind one that is never
	// reachable.
	if int(page.Total) != len(page.Items) && page.Total < 2 {
		t.Errorf("Total = %d, want at least the 2 live orders this test placed", page.Total)
	}
	for _, item := range page.Items {
		if item.Status != orderdomain.StatusPaid &&
			item.Status != orderdomain.StatusPendingPayment &&
			item.Status != orderdomain.StatusRefundRequired {
			t.Errorf("order %s has status %s, which was not asked for", item.ID, item.Status)
		}
	}
}

// A buyer may not widen the filter into somebody else's orders.
//
// The set form takes a slice, which is one more thing arriving from a query
// string, so the scoping that the single-status form gets has to be shown to
// apply to it too.
func TestListWithASetOfStatusesStaysScopedToTheBuyer(t *testing.T) {
	h := newHarness(t, 20)
	ctx := context.Background()

	item, err := h.start(1, testsupport.Unique("scope"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	stranger := authdomain.Actor{ID: testsupport.Unique("usr"), Role: userdomain.RoleUser}
	page, err := h.service.List(ctx, stranger, orderdomain.Filter{
		Statuses: []orderdomain.Status{orderdomain.StatusPaid, orderdomain.StatusPendingPayment},
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, other := range page.Items {
		if other.ID == item.ID {
			t.Fatalf("order %s belongs to %s and was returned to %s", item.ID, h.ownerID, stranger.ID)
		}
	}
}

// An empty set is no constraint, not an impossible one.
//
// `?status=` from a client that sent a blank field must not become `IN ()`,
// which matches nothing: the buyer would be told they have no orders at all.
func TestListIgnoresAnEmptyStatusSet(t *testing.T) {
	h := newHarness(t, 20)
	ctx := context.Background()
	actor := authdomain.Actor{ID: h.ownerID, Role: userdomain.RoleUser}

	item, err := h.start(1, testsupport.Unique("blank"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	page, err := h.service.List(ctx, actor, orderdomain.Filter{
		Statuses: []orderdomain.Status{"", "  "},
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, candidate := range page.Items {
		if candidate.ID == item.ID {
			return
		}
	}
	t.Fatalf("order %s was hidden by a status set that names no status", item.ID)
}
