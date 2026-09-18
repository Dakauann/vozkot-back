package payout

import (
	"strings"
	"testing"
)

// The id column is varchar(32), and an entry id that overflows it fails the
// INSERT inside the transaction that settles a payment, after the money has
// already moved at the provider.
//
// The first version of this concatenated the order id and a counter, which fit
// a production order id and overflowed the moment a longer one appeared. The
// full suite caught it; this keeps it caught.
func TestADerivedEntryIDAlwaysFitsTheColumn(t *testing.T) {
	const columnWidth = 32
	for _, orderID := range []string{
		"ord_9f2c",
		"ord_beb5710d04eb3765",
		"ord_1789676246378747600_130",
		"ord_" + strings.Repeat("x", 200),
	} {
		next := derivedIDs(orderID)
		for n := 0; n < 5; n++ {
			id := next()
			if len(id) > columnWidth {
				t.Fatalf("id %q for order %q is %d characters, column holds %d",
					id, orderID, len(id), columnWidth)
			}
			if !strings.HasPrefix(id, "led_") {
				t.Errorf("id %q does not carry the ledger prefix", id)
			}
		}
	}
}

// Idempotency rests entirely on this: a redelivered webhook has to compute the
// SAME ids, or ON CONFLICT has nothing to discard and the organiser is paid
// twice for one order.
func TestDerivedEntryIDsRepeatForTheSameOrderAndDifferForOthers(t *testing.T) {
	first, second := derivedIDs("ord_9f2c"), derivedIDs("ord_9f2c")
	other := derivedIDs("ord_7b3d")

	seen := map[string]bool{}
	for n := 0; n < 4; n++ {
		a, b, c := first(), second(), other()
		if a != b {
			t.Fatalf("entry %d of the same order got %q and %q", n, a, b)
		}
		if a == c {
			t.Fatalf("two different orders produced the same id %q", a)
		}
		if seen[a] {
			t.Fatalf("id %q was produced twice for one order", a)
		}
		seen[a] = true
	}
}
