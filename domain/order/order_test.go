package order

import (
	"errors"
	"testing"
	"time"

	"vozkot/domain/payment"
	"vozkot/domain/pricing"
	"vozkot/domain/refund"
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
		RefundPolicyVersion: refund.CurrentPolicyVersion,
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
	// No fee configured, so the buyer owes exactly the face value and the
	// organiser is owed all of it.
	if item.SubtotalCents != 48000 || item.BuyerFeeCents != 0 {
		t.Fatalf("subtotal/fee = %d/%d, want 48000/0", item.SubtotalCents, item.BuyerFeeCents)
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

// The service fee rides on top of the face value: the buyer pays more, the
// organiser is owed exactly what they priced the tier at. This is the whole
// point of keeping SubtotalCents and BuyerFeeCents apart, so it is pinned here.
func TestTheServiceFeeIsAddedOnTopAndNeverTakenFromTheOrganiser(t *testing.T) {
	draft := validDraft()
	draft.Fee = pricing.Fee{BasisPoints: pricing.PlatformBasisPoints}

	item, err := New("ord_1", draft, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if item.SubtotalCents != 48_000 {
		t.Fatalf("the organiser's share = %d, want 48000; the fee must not come out of it", item.SubtotalCents)
	}
	if item.BuyerFeeCents != 4_800 {
		t.Fatalf("fee = %d, want 4800 (10%% of 48000)", item.BuyerFeeCents)
	}
	if item.TotalCents != 52_800 {
		t.Fatalf("charged = %d, want 52800 (48000 + 4800)", item.TotalCents)
	}
	if item.TotalCents != item.SubtotalCents+item.BuyerFeeCents {
		t.Fatal("the total is not the subtotal plus the fee")
	}

	line := item.Items[0]
	if line.TotalCents != 48_000 || line.UnitFeeCents != 2_400 || line.FeeCents != 4_800 {
		t.Fatalf("line = face %d, unit fee %d, fee %d; want 48000 / 2400 / 4800",
			line.TotalCents, line.UnitFeeCents, line.FeeCents)
	}
	if line.ChargedCents() != 52_800 {
		t.Fatalf("line charged = %d, want 52800", line.ChargedCents())
	}
}

// A free ticket stays free. A service fee appearing on a free-admission event
// would be the most visible way this could be wrong.
func TestAFreeTicketCarriesNoFee(t *testing.T) {
	draft := validDraft()
	draft.Fee = pricing.Fee{BasisPoints: pricing.PlatformBasisPoints}
	draft.Items[0].UnitPriceCents = 0

	item, err := New("ord_1", draft, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.TotalCents != 0 || item.BuyerFeeCents != 0 {
		t.Fatalf("a free order cost %d with %d of fee", item.TotalCents, item.BuyerFeeCents)
	}
}

// The order totals must be the sum of the lines, across several tiers, or a
// receipt and a charge disagree.
func TestTotalsAreTheSumOfTheLines(t *testing.T) {
	draft := validDraft()
	draft.Fee = pricing.Fee{BasisPoints: pricing.PlatformBasisPoints}
	draft.Items = []Item{
		{TicketID: "tkt_1", TicketTitle: "Pista", Quantity: 2, UnitPriceCents: 3_333},
		{TicketID: "tkt_2", TicketTitle: "Camarote", Quantity: 1, UnitPriceCents: 12_500},
	}

	item, err := New("ord_1", draft, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var face, fee int64
	for _, line := range item.Items {
		face += line.TotalCents
		fee += line.FeeCents
	}
	if item.SubtotalCents != face {
		t.Fatalf("subtotal %d is not the sum of the lines %d", item.SubtotalCents, face)
	}
	if item.BuyerFeeCents != fee {
		t.Fatalf("fee %d is not the sum of the line fees %d", item.BuyerFeeCents, fee)
	}
	if item.TotalCents != face+fee {
		t.Fatalf("total %d is not %d + %d", item.TotalCents, face, fee)
	}
}

// An order with no cancellation rules frozen onto it cannot answer the one
// question a buyer will ask, so it is refused rather than defaulted.
func TestAnOrderMustCarryItsRefundPolicy(t *testing.T) {
	draft := validDraft()
	draft.RefundPolicyVersion = 0

	if _, err := New("ord_1", draft, time.Minute, time.Now()); !errors.Is(err, ErrNoRefundPolicy) {
		t.Fatalf("New() error = %v, want %v", err, ErrNoRefundPolicy)
	}
}

// The demographics are frozen at purchase, like the tier title and the price.
func TestTheBuyerSnapshotIsNormalisedAndKept(t *testing.T) {
	draft := validDraft()
	draft.BuyerGender = "female"
	draft.BuyerAgeYears = 31
	draft.BuyerCity = "  Natal "
	draft.BuyerUF = "rn"

	item, err := New("ord_1", draft, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.BuyerGender != "female" || item.BuyerAgeYears != 31 {
		t.Fatalf("gender/age = %q/%d, want female/31", item.BuyerGender, item.BuyerAgeYears)
	}
	if item.BuyerCity != "Natal" || item.BuyerUF != "RN" {
		t.Fatalf("city/uf = %q/%q, want Natal/RN", item.BuyerCity, item.BuyerUF)
	}
}

// ValidatePricing is the guard on an order that came back from storage rather
// than out of New. These are the shapes a bad backfill, a mixed-version deploy
// or a hand-edited row can leave behind, and every one of them would otherwise
// bill the buyer one number and credit the organiser from another.
func TestValidatePricingRejectsMoneyThatDoesNotAddUp(t *testing.T) {
	line := func(face, fee int64) Item {
		return Item{TicketID: "tkt_1", Quantity: 1, TotalCents: face, FeeCents: fee}
	}

	cases := []struct {
		name  string
		order Order
	}{
		{
			// The one that matters most: the total is not the sum, so the
			// charge and the payout disagree by 500 centavos.
			name: "total is not subtotal plus fee",
			order: Order{
				ID: "ord_1", SubtotalCents: 10_000, BuyerFeeCents: 1_000, TotalCents: 10_500,
				Items: []Item{line(10_000, 1_000)},
			},
		},
		{
			name: "lines do not sum to the subtotal",
			order: Order{
				ID: "ord_2", SubtotalCents: 10_000, BuyerFeeCents: 1_000, TotalCents: 11_000,
				Items: []Item{line(9_000, 1_000)},
			},
		},
		{
			name: "lines do not sum to the fee",
			order: Order{
				ID: "ord_3", SubtotalCents: 10_000, BuyerFeeCents: 1_000, TotalCents: 11_000,
				Items: []Item{line(10_000, 900)},
			},
		},
		{
			// A negative fee is a refund wearing a charge's clothes.
			name: "a negative fee",
			order: Order{
				ID: "ord_4", SubtotalCents: 10_000, BuyerFeeCents: -1_000, TotalCents: 9_000,
				Items: []Item{line(10_000, -1_000)},
			},
		},
		{
			name: "a negative total",
			order: Order{
				ID: "ord_5", SubtotalCents: -10_000, BuyerFeeCents: 0, TotalCents: -10_000,
				Items: []Item{line(-10_000, 0)},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.order.ValidatePricing()
			if !errors.Is(err, ErrPricingInconsistent) {
				t.Fatalf("ValidatePricing() = %v, want ErrPricingInconsistent", err)
			}
		})
	}
}

func TestValidatePricingAcceptsWhatNewProduces(t *testing.T) {
	draft := validDraft()
	draft.Fee = pricing.Fee{BasisPoints: pricing.PlatformBasisPoints}
	draft.Items = []Item{
		{TicketID: "tkt_1", TicketTitle: "Mezanino", Quantity: 3, UnitPriceCents: 3_335},
		{TicketID: "tkt_2", TicketTitle: "Pista", Quantity: 2, UnitPriceCents: 24_000},
	}

	item, err := New("ord_ok", draft, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if err := item.ValidatePricing(); err != nil {
		t.Fatalf("an order New built failed its own invariant: %v", err)
	}

	// A free order is consistent too: every column is zero and the guard must
	// not read that as a fault.
	free := validDraft()
	free.Fee = pricing.Fee{BasisPoints: pricing.PlatformBasisPoints}
	free.Items = []Item{{TicketID: "tkt_1", TicketTitle: "Convite", Quantity: 2, UnitPriceCents: 0}}
	gift, err := New("ord_free", free, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("New() for a free order: %v", err)
	}
	if err := gift.ValidatePricing(); err != nil {
		t.Fatalf("a free order failed the invariant: %v", err)
	}
	if gift.TotalCents != 0 {
		t.Fatalf("a free order was charged %d", gift.TotalCents)
	}
}

// An order read back without its lines is not a pricing fault: a query that
// did not ask for them still returns authoritative totals.
func TestValidatePricingAllowsAnOrderWithoutLines(t *testing.T) {
	item := Order{ID: "ord_6", SubtotalCents: 10_000, BuyerFeeCents: 1_000, TotalCents: 11_000}
	if err := item.ValidatePricing(); err != nil {
		t.Fatalf("ValidatePricing() on a line-less order = %v, want nil", err)
	}
}

// Reference is what a buyer reads out to support and what a doorperson reads
// off a scan. Short enough to say, long enough to find the row, and — the
// reason it lives here — the SAME on both surfaces.
func TestReference(t *testing.T) {
	cases := map[string]string{
		"ord_a1b2c3d4e5f6": "A1B2C3D4",
		"ord_ab":           "AB",
		"ord_":             "",
		// An id that never carried the prefix still reduces sensibly.
		"a1b2c3d4e5f6": "A1B2C3D4",
	}
	for id, want := range cases {
		if got := Reference(id); got != want {
			t.Errorf("Reference(%q) = %q, want %q", id, got, want)
		}
	}

	// The method and the function must agree; two surfaces read this number
	// aloud to each other.
	item := &Order{ID: "ord_a1b2c3d4e5f6"}
	if item.Reference() != Reference(item.ID) {
		t.Fatalf("Order.Reference() = %q but Reference() = %q", item.Reference(), Reference(item.ID))
	}
	if len(item.Reference()) > ReferenceLength {
		t.Fatalf("Reference() is %d characters, longer than the documented %d", len(item.Reference()), ReferenceLength)
	}
}
