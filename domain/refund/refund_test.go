package refund

import (
	"testing"
	"time"
)

func instant(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

// paid is an order bought on the 1st, refundable by default.
func paid() Order {
	return Order{
		ID:            "ord_1",
		Paid:          true,
		PurchasedAt:   instant("2026-09-01T12:00:00Z"),
		PolicyVersion: CurrentPolicyVersion,
	}
}

// The shipped policy must satisfy the law. If somebody edits the numbers, this
// is the test that refuses the edit.
func TestCurrentPolicyIsLawful(t *testing.T) {
	policy := CurrentPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("the shipped policy is not lawful: %v", err)
	}
	if policy.WithdrawalDays < StatutoryWithdrawalDays {
		t.Fatalf("withdrawal window is %d days, below the statutory %d",
			policy.WithdrawalDays, StatutoryWithdrawalDays)
	}
	if !policy.RefundsFees {
		t.Fatal("the policy keeps the service fee; Procon-SP and the STJ say it must come back")
	}
	if _, err := PolicyFor(CurrentPolicyVersion); err != nil {
		t.Fatalf("CurrentPolicyVersion %d has no policy: %v", CurrentPolicyVersion, err)
	}
}

func TestValidateRefusesAnIllegalPolicy(t *testing.T) {
	cases := map[string]Policy{
		"fewer than seven days": {WithdrawalDays: 6, RefundsFees: true},
		"keeps the fee":         {WithdrawalDays: 7, RefundsFees: false},
		"negative cutoff":       {WithdrawalDays: 7, CutoffBeforeEvent: -time.Hour, RefundsFees: true},
	}
	for name, policy := range cases {
		if err := policy.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestWithdrawalIsAllowedInsideBothClocks(t *testing.T) {
	policy := CurrentPolicy()
	event := EventTiming{StartsAt: instant("2026-12-01T20:00:00Z")}

	decision := Evaluate(policy, paid(), event, ReasonBuyerWithdrawal, instant("2026-09-03T09:00:00Z"))
	if !decision.Allowed {
		t.Fatalf("a withdrawal two days after purchase was refused: %s", decision.Refusal)
	}
	// The event is months away, so the seven-day window is the binding clock.
	if want := instant("2026-09-08T12:00:00Z"); !decision.Until.Equal(want) {
		t.Fatalf("deadline = %s, want %s (purchase + 7 days)", decision.Until, want)
	}
}

func TestWithdrawalClosesSevenDaysAfterPurchase(t *testing.T) {
	policy := CurrentPolicy()
	event := EventTiming{StartsAt: instant("2026-12-01T20:00:00Z")}

	// The instant the window closes is already too late; the deadline is
	// exclusive, so "until the 8th at 12:00" means 11:59:59 works and 12:00
	// does not.
	decision := Evaluate(policy, paid(), event, ReasonBuyerWithdrawal, instant("2026-09-08T12:00:00Z"))
	if decision.Allowed {
		t.Fatal("a withdrawal was allowed at the exact instant the window closed")
	}
	if decision.Refusal != RefusalWindowClosed {
		t.Fatalf("refusal = %q, want %q", decision.Refusal, RefusalWindowClosed)
	}
}

// The cutoff only ever brings the deadline forward. A buyer who bought two days
// before the show does not get seven days.
func TestTheCutoffBeatsTheWindowWhenTheEventIsClose(t *testing.T) {
	policy := CurrentPolicy()
	order := paid()
	order.PurchasedAt = instant("2026-09-01T12:00:00Z")
	event := EventTiming{StartsAt: instant("2026-09-04T20:00:00Z")}

	// 48 h before the doors is the 2nd at 20:00, well inside the seven days.
	allowed := Evaluate(policy, order, event, ReasonBuyerWithdrawal, instant("2026-09-02T19:00:00Z"))
	if !allowed.Allowed {
		t.Fatalf("refused an hour before the cutoff: %s", allowed.Refusal)
	}
	if want := instant("2026-09-02T20:00:00Z"); !allowed.Until.Equal(want) {
		t.Fatalf("deadline = %s, want %s (doors − 48h)", allowed.Until, want)
	}

	refused := Evaluate(policy, order, event, ReasonBuyerWithdrawal, instant("2026-09-02T21:00:00Z"))
	if refused.Allowed {
		t.Fatal("a withdrawal was allowed inside the 48-hour cutoff")
	}
	if refused.Refusal != RefusalTooCloseToEvent {
		t.Fatalf("refusal = %q, want %q", refused.Refusal, RefusalTooCloseToEvent)
	}
}

// Buying inside the cutoff means no self-service cancellation at all, which is
// Sympla's rule and the one the doc adopts.
func TestBuyingInsideTheCutoffLeavesNoWindow(t *testing.T) {
	policy := CurrentPolicy()
	order := paid()
	order.PurchasedAt = instant("2026-09-04T09:00:00Z")
	event := EventTiming{StartsAt: instant("2026-09-04T20:00:00Z")}

	decision := Evaluate(policy, order, event, ReasonBuyerWithdrawal, instant("2026-09-04T09:01:00Z"))
	if decision.Allowed {
		t.Fatal("a ticket bought eleven hours before the doors was still cancellable")
	}
	if decision.Refusal != RefusalTooCloseToEvent {
		t.Fatalf("refusal = %q, want %q", decision.Refusal, RefusalTooCloseToEvent)
	}
}

func TestAPassedEventIsNotRefundable(t *testing.T) {
	policy := CurrentPolicy()
	order := paid()
	order.PurchasedAt = instant("2026-08-30T12:00:00Z")
	event := EventTiming{StartsAt: instant("2026-09-01T20:00:00Z")}

	decision := Evaluate(policy, order, event, ReasonBuyerWithdrawal, instant("2026-09-02T09:00:00Z"))
	if decision.Allowed {
		t.Fatal("an event that already happened was refundable by withdrawal")
	}
	if decision.Refusal != RefusalEventPassed {
		t.Fatalf("refusal = %q, want %q", decision.Refusal, RefusalEventPassed)
	}
}

// A cancelled event overrides every clock. This is the case a window must never
// be allowed to refuse.
func TestACancelledEventIsAlwaysRefundable(t *testing.T) {
	policy := CurrentPolicy()
	order := paid()
	order.PurchasedAt = instant("2026-01-01T12:00:00Z")
	// Months past the window, hours before a show that is now off.
	event := EventTiming{StartsAt: instant("2026-09-02T20:00:00Z"), Cancelled: true}

	decision := Evaluate(policy, order, event, ReasonBuyerWithdrawal, instant("2026-09-02T09:00:00Z"))
	if !decision.Allowed {
		t.Fatalf("a cancelled event refused a refund: %s", decision.Refusal)
	}
	if !decision.Until.IsZero() {
		t.Fatalf("a cancelled event carried a deadline %s; it has no clock", decision.Until)
	}
}

// Even an event that already happened refunds if it was cancelled: the doors
// never opened, whatever the calendar says.
func TestACancelledEventRefundsAfterItsOwnDate(t *testing.T) {
	policy := CurrentPolicy()
	event := EventTiming{StartsAt: instant("2026-09-02T20:00:00Z"), Cancelled: true}

	decision := Evaluate(policy, paid(), event, ReasonEventCancelled, instant("2026-09-20T09:00:00Z"))
	if !decision.Allowed {
		t.Fatalf("a cancelled event stopped refunding once its date passed: %s", decision.Refusal)
	}
}

func TestAnUnpaidOrCompletedOrderIsRefusedBeforeAnyWindow(t *testing.T) {
	policy := CurrentPolicy()
	event := EventTiming{StartsAt: instant("2026-12-01T20:00:00Z")}
	now := instant("2026-09-02T09:00:00Z")

	unpaid := paid()
	unpaid.Paid = false
	if decision := Evaluate(policy, unpaid, event, ReasonBuyerWithdrawal, now); decision.Refusal != RefusalNotPaid {
		t.Fatalf("unpaid refusal = %q, want %q", decision.Refusal, RefusalNotPaid)
	}

	done := paid()
	done.AlreadyRefunded = true
	if decision := Evaluate(policy, done, event, ReasonOperator, now); decision.Refusal != RefusalAlreadyRefunded {
		t.Fatalf("refunded refusal = %q, want %q", decision.Refusal, RefusalAlreadyRefunded)
	}
}

func TestAnOpenRequestBlocksASecondOne(t *testing.T) {
	policy := CurrentPolicy()
	order := paid()
	order.HasOpenRequest = true
	event := EventTiming{StartsAt: instant("2026-12-01T20:00:00Z")}

	decision := Evaluate(policy, order, event, ReasonBuyerWithdrawal, instant("2026-09-02T09:00:00Z"))
	if decision.Allowed {
		t.Fatal("a second refund request was allowed while one was in flight")
	}
	if decision.Refusal != RefusalRequestOpen {
		t.Fatalf("refusal = %q, want %q", decision.Refusal, RefusalRequestOpen)
	}
}

// The organiser and an operator are not bound by the buyer's window. That is
// the difference between a right and a favour.
func TestOperatorAndGoodwillIgnoreTheWindow(t *testing.T) {
	policy := CurrentPolicy()
	order := paid()
	order.PurchasedAt = instant("2026-01-01T12:00:00Z")
	event := EventTiming{StartsAt: instant("2026-12-01T20:00:00Z")}
	now := instant("2026-09-02T09:00:00Z")

	for _, reason := range []Reason{ReasonOperator, ReasonOrganiserGoodwill} {
		if decision := Evaluate(policy, order, event, reason, now); !decision.Allowed {
			t.Errorf("%s was refused long past the buyer's window: %s", reason, decision.Refusal)
		}
	}
}

// A buyer must not be able to pick grounds that skip the window.
func TestOnlyWithdrawalAndCancellationAreBuyerClaimable(t *testing.T) {
	claimable := map[Reason]bool{
		ReasonBuyerWithdrawal:   true,
		ReasonEventCancelled:    true,
		ReasonOrganiserGoodwill: false,
		ReasonOperator:          false,
	}
	for reason, want := range claimable {
		if got := reason.BuyerMayClaim(); got != want {
			t.Errorf("%s.BuyerMayClaim() = %v, want %v", reason, got, want)
		}
	}
}

// An unknown event date must not trap a buyer's money.
func TestAnUnknownEventDateAppliesNoCutoff(t *testing.T) {
	policy := CurrentPolicy()
	decision := Evaluate(policy, paid(), EventTiming{}, ReasonBuyerWithdrawal, instant("2026-09-03T09:00:00Z"))
	if !decision.Allowed {
		t.Fatalf("an order whose event could not be read refused a withdrawal: %s", decision.Refusal)
	}
	if want := instant("2026-09-08T12:00:00Z"); !decision.Until.Equal(want) {
		t.Fatalf("deadline = %s, want the window alone (%s)", decision.Until, want)
	}
}

func TestNewAutoApprovesOnlyTheUndeclinableGrounds(t *testing.T) {
	now := instant("2026-09-02T09:00:00Z")
	draft := Draft{OrderID: "ord_1", AmountCents: 11_000, FeeCents: 1_000}

	// Operator joined these when settlement started opening its own refunds.
	// The other two are refunds nobody has STANDING to refuse; this one is a
	// refund nobody is being ASKED about, because it is the box office's own
	// mistake. Leaving it pending put a buyer whose seat we resold behind
	// whoever next opened an inbox.
	for _, reason := range []Reason{ReasonBuyerWithdrawal, ReasonEventCancelled, ReasonOperator} {
		draft.Reason = reason
		request, err := New("rfr_1", draft, now)
		if err != nil {
			t.Fatalf("New(%s): %v", reason, err)
		}
		if request.Status != StatusApproved {
			t.Errorf("%s opened as %q, want %q", reason, request.Status, StatusApproved)
		}
		if !request.AutoApproved() {
			t.Errorf("%s was not recorded as decided by the policy", reason)
		}
		if request.DecidedBy != "" {
			t.Errorf("%s named %q as the decider; nobody decided it", reason, request.DecidedBy)
		}
	}

	// Goodwill is the one that still waits: it is an organiser choosing to
	// give money back when they do not have to, which is a decision and not a
	// consequence.
	for _, reason := range []Reason{ReasonOrganiserGoodwill} {
		draft.Reason = reason
		request, err := New("rfr_1", draft, now)
		if err != nil {
			t.Fatalf("New(%s): %v", reason, err)
		}
		if request.Status != StatusPending {
			t.Errorf("%s opened as %q, want %q", reason, request.Status, StatusPending)
		}
	}
}

func TestNewRejectsNonsense(t *testing.T) {
	now := instant("2026-09-02T09:00:00Z")
	if _, err := New("rfr_1", Draft{OrderID: "o", Reason: "whatever", AmountCents: 100}, now); err != ErrInvalidReason {
		t.Errorf("an unknown reason was accepted: %v", err)
	}
	if _, err := New("rfr_1", Draft{OrderID: "o", Reason: ReasonOperator, AmountCents: 0}, now); err != ErrInvalidAmount {
		t.Errorf("a zero refund was accepted: %v", err)
	}
}

// A second decision is an error rather than a silent no-op: it is a second
// person disagreeing with the first, and swallowing it would lose that.
func TestARequestIsDecidedOnce(t *testing.T) {
	now := instant("2026-09-02T09:00:00Z")
	request, err := New("rfr_1", Draft{
		OrderID: "ord_1", Reason: ReasonOrganiserGoodwill, AmountCents: 11_000,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := request.Decide(StatusApproved, "usr_org", "ok", now); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	if request.DecidedBy != "usr_org" || request.DecidedAt == nil {
		t.Fatalf("the decision was not recorded: %+v", request)
	}
	if err := request.Decide(StatusRejected, "usr_other", "no", now); err != ErrNotDecidable {
		t.Fatalf("a decided request was decided again: %v", err)
	}
	if request.Status != StatusApproved {
		t.Fatalf("the second decision overwrote the first: %q", request.Status)
	}
}

func TestDecideRefusesAStatusThatIsNotADecision(t *testing.T) {
	now := instant("2026-09-02T09:00:00Z")
	request, _ := New("rfr_1", Draft{OrderID: "o", Reason: ReasonOperator, AmountCents: 100}, now)
	if err := request.Decide(StatusPending, "usr", "", now); err != ErrNotDecidable {
		t.Fatalf("pending was accepted as a decision: %v", err)
	}
}

func TestNoteIsBounded(t *testing.T) {
	now := instant("2026-09-02T09:00:00Z")
	long := make([]rune, MaxNoteRunes+500)
	for index := range long {
		long[index] = 'a'
	}
	request, err := New("rfr_1", Draft{
		OrderID: "o", Reason: ReasonOperator, AmountCents: 100, Note: string(long),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(request.Note)) != MaxNoteRunes {
		t.Fatalf("note kept %d runes, want %d", len([]rune(request.Note)), MaxNoteRunes)
	}
}
