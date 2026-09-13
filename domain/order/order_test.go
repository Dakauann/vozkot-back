package order

import (
	"errors"
	"testing"
	"time"

	"vozkot/domain/payment"
)

func validDraft() Draft {
	return Draft{
		EventID:       "evt_1",
		BuyerName:     "Maria Souza",
		BuyerEmail:    "maria@exemplo.com.br",
		BuyerDocument: "123.456.789-09",
		Items: []Item{
			{TicketID: "tkt_1", TicketTitle: "Pista", Quantity: 2, UnitPriceCents: 24000},
		},
	}
}

func TestNewHoldsStockAndTotalsTheMoney(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	item, err := New("ord_1", validDraft(), 30*time.Minute, now)

	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.Status != StatusPendingPayment || !item.Status.HoldsStock() {
		t.Fatalf("status = %q, want a status that holds stock", item.Status)
	}
	if item.TotalCents != 48000 {
		t.Fatalf("total = %d, want 48000 (2 x 24000)", item.TotalCents)
	}
	if !item.HoldExpiresAt.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("hold expires at %v, want %v", item.HoldExpiresAt, now.Add(30*time.Minute))
	}
	if item.BuyerDocument != "12345678909" {
		t.Fatalf("document = %q, want the digits only", item.BuyerDocument)
	}
	if item.PaymentMethod != payment.MethodPix {
		t.Fatalf("method = %q, want pix by default", item.PaymentMethod)
	}
}

func TestNewRejectsWhatCannotBeCharged(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Draft)
		want   error
	}{
		"no event":       {func(d *Draft) { d.EventID = " " }, ErrInvalidEvent},
		"no ticket":      {func(d *Draft) { d.Items[0].TicketID = " " }, ErrInvalidTicket},
		"no items":       {func(d *Draft) { d.Items = nil }, ErrInvalidQuantity},
		"zero quantity":  {func(d *Draft) { d.Items[0].Quantity = 0 }, ErrInvalidQuantity},
		"hoarding":       {func(d *Draft) { d.Items[0].Quantity = MaxQuantityPerOrder + 1 }, ErrInvalidQuantity},
		"no buyer name":  {func(d *Draft) { d.BuyerName = "  " }, ErrInvalidBuyerName},
		"bad email":      {func(d *Draft) { d.BuyerEmail = "maria@" }, ErrInvalidBuyerEmail},
		"negative price": {func(d *Draft) { d.Items[0].UnitPriceCents = -1 }, payment.ErrInvalidAmount},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			draft := validDraft()
			testCase.mutate(&draft)

			if _, err := New("ord_1", draft, time.Minute, time.Now()); !errors.Is(err, testCase.want) {
				t.Fatalf("New() error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	item, _ := New("ord_1", validDraft(), time.Minute, time.Now())
	if _, err := item.Apply(StatusPaid, time.Now()); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	paidAt := item.PaidAt

	changed, err := item.Apply(StatusPaid, time.Now().Add(time.Minute))

	if err != nil {
		t.Fatalf("second Apply() error = %v, want nil: a redelivered approval is not an error", err)
	}
	if changed {
		t.Fatal("second Apply() reported a change; stock would move twice")
	}
	if item.PaidAt != paidAt {
		t.Fatal("re-applying paid moved the payment timestamp")
	}
}

func TestApplyRefusesImpossibleTransitions(t *testing.T) {
	cases := map[string]struct {
		from Status
		to   Status
	}{
		"refunded cannot be paid again": {StatusRefunded, StatusPaid},
		"paid cannot expire":            {StatusPaid, StatusExpired},
		"expired cannot be cancelled":   {StatusExpired, StatusCancelled},
		"paid cannot fail":              {StatusPaid, StatusFailed},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			item, _ := New("ord_1", validDraft(), time.Minute, time.Now())
			item.Status = testCase.from

			if _, err := item.Apply(testCase.to, time.Now()); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("Apply(%s -> %s) error = %v, want %v", testCase.from, testCase.to, err, ErrInvalidTransition)
			}
			if item.Status != testCase.from {
				t.Fatalf("status moved to %q on a refused transition", item.Status)
			}
		})
	}
}

func TestLapsedHoldCanStillBeSettled(t *testing.T) {
	// A buyer who pays eleven minutes into a ten-minute hold is a real and
	// frequent case, and the state machine has to allow the order to be
	// resolved one way or the other rather than stranding the money.
	item, _ := New("ord_1", validDraft(), time.Minute, time.Now())
	if _, err := item.Apply(StatusExpired, time.Now()); err != nil {
		t.Fatalf("Apply(expired) error = %v", err)
	}

	if !item.CanTransition(StatusPaid) {
		t.Fatal("an expired order must still be payable when the stock can be re-reserved")
	}
	if !item.CanTransition(StatusRefundRequired) {
		t.Fatal("an expired order must be able to say a refund is owed")
	}
}

func TestHoldLapsedOnlyWhileHoldingStock(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	item, _ := New("ord_1", validDraft(), 10*time.Minute, now)

	if item.HoldLapsed(now.Add(9 * time.Minute)) {
		t.Fatal("hold reported as lapsed before its window closed")
	}
	if !item.HoldLapsed(now.Add(10 * time.Minute)) {
		t.Fatal("hold did not lapse at its expiry")
	}

	if _, err := item.Apply(StatusPaid, now); err != nil {
		t.Fatalf("Apply(paid) error = %v", err)
	}
	if item.HoldLapsed(now.Add(time.Hour)) {
		t.Fatal("a paid order still reports a lapsed hold; the sweep would release sold stock")
	}
}

func TestStatusForMapsEveryChargeState(t *testing.T) {
	cases := map[payment.Status]struct {
		want  Status
		moves bool
	}{
		payment.StatusPaid:        {StatusPaid, true},
		payment.StatusRejected:    {StatusFailed, true},
		payment.StatusCancelled:   {StatusExpired, true},
		payment.StatusRefunded:    {StatusRefunded, true},
		payment.StatusChargedBack: {StatusRefunded, true},
		// Pending and in-analysis must move nothing: the money is not ours.
		payment.StatusPending:    {"", false},
		payment.StatusInAnalysis: {"", false},
	}

	for chargeStatus, expected := range cases {
		t.Run(string(chargeStatus), func(t *testing.T) {
			got, moves := StatusFor(chargeStatus)
			if moves != expected.moves || got != expected.want {
				t.Fatalf("StatusFor(%q) = (%q, %t), want (%q, %t)",
					chargeStatus, got, moves, expected.want, expected.moves)
			}
		})
	}
}

func TestAttachChargeDoesNotMoveTheOrder(t *testing.T) {
	item, _ := New("ord_1", validDraft(), time.Minute, time.Now())

	item.AttachCharge(&payment.Charge{
		ID:           "1234567890",
		Provider:     payment.ProviderMercadoPago,
		Status:       payment.StatusPending,
		Method:       payment.MethodPix,
		PixCopyPaste: "00020126",
	}, time.Now())

	if item.Status != StatusPendingPayment {
		t.Fatalf("status = %q; a created charge is still an unpaid order", item.Status)
	}
	if item.PaymentID != "1234567890" || item.PixCopyPaste != "00020126" {
		t.Fatalf("charge details were not recorded: %+v", item)
	}
}
