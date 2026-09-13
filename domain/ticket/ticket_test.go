package ticket

import (
	"errors"
	"testing"
	"time"
)

func validDraft() Draft {
	return Draft{
		EventID:    "evt_test",
		Title:      "Pista Premium",
		PriceCents: 24000,
		Quantity:   500,
	}
}

func TestNewStartsAsDraftInBRL(t *testing.T) {
	now := time.Now()

	item, err := New("tkt_1", "usr_1", validDraft(), now)

	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.Status != StatusDraft {
		t.Fatalf("status = %q, want %q: a new ticket must not reach buyers before someone publishes it", item.Status, StatusDraft)
	}
	if item.Currency != DefaultCurrency {
		t.Fatalf("currency = %q, want %q", item.Currency, DefaultCurrency)
	}
	if item.Sold != 0 || item.Available() != 500 {
		t.Fatalf("sold = %d, available = %d, want 0 and 500", item.Sold, item.Available())
	}
}

func TestNewRejectsIncompleteDrafts(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Draft)
		want   error
	}{
		"no event":       {func(d *Draft) { d.EventID = "  " }, ErrInvalidEvent},
		"no title":       {func(d *Draft) { d.Title = "" }, ErrInvalidTitle},
		"negative price": {func(d *Draft) { d.PriceCents = -1 }, ErrInvalidPrice},
		"no stock":       {func(d *Draft) { d.Quantity = 0 }, ErrInvalidQuantity},
		"bad status":     {func(d *Draft) { d.Status = "on-sale" }, ErrInvalidStatus},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			draft := validDraft()
			testCase.mutate(&draft)

			if _, err := New("tkt_1", "usr_1", draft, time.Now()); !errors.Is(err, testCase.want) {
				t.Fatalf("New() error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestApplyRefusesQuantityBelowSold(t *testing.T) {
	item, err := New("tkt_1", "usr_1", validDraft(), time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	item.Sold = 120

	draft := validDraft()
	draft.Quantity = 100

	if err := item.Apply(draft, time.Now()); !errors.Is(err, ErrQuantityBelowSold) {
		t.Fatalf("Apply() error = %v, want %v: shrinking below sales would promise refunds the box office cannot honour", err, ErrQuantityBelowSold)
	}
	if item.Quantity != 500 {
		t.Fatalf("quantity = %d, want 500: a rejected edit must not partially apply", item.Quantity)
	}
}

func TestApplyKeepsCurrentStatusWhenOmitted(t *testing.T) {
	item, _ := New("tkt_1", "usr_1", validDraft(), time.Now())
	if err := item.ChangeStatus(StatusOnSale, time.Now()); err != nil {
		t.Fatalf("ChangeStatus() error = %v", err)
	}

	draft := validDraft()
	draft.Title = "Camarote"

	if err := item.Apply(draft, time.Now()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if item.Status != StatusOnSale {
		t.Fatalf("status = %q, want %q: editing a title must not quietly unpublish a listing", item.Status, StatusOnSale)
	}
}

func TestChangeStatusRefusesSellingWithoutStock(t *testing.T) {
	item, _ := New("tkt_1", "usr_1", validDraft(), time.Now())
	item.Sold = item.Quantity

	if err := item.ChangeStatus(StatusOnSale, time.Now()); !errors.Is(err, ErrNoStock) {
		t.Fatalf("ChangeStatus() error = %v, want %v", err, ErrNoStock)
	}
	if err := item.ChangeStatus(StatusSoldOut, time.Now()); err != nil {
		t.Fatalf("ChangeStatus(sold_out) error = %v", err)
	}
}

func TestAvailableNeverGoesNegative(t *testing.T) {
	item, _ := New("tkt_1", "usr_1", validDraft(), time.Now())
	item.Sold = 600

	if got := item.Available(); got != 0 {
		t.Fatalf("Available() = %d, want 0", got)
	}
}
