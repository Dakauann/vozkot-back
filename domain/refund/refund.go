// Package refund is when a buyer may have their money back, how much of it,
// and the record of who decided.
//
// The whole design turns on one idea from docs/REFUNDS_AND_PAYOUTS.md: the
// GROUNDS decide everything. A buyer exercising their statutory right of
// withdrawal, an organiser calling the show off, an organiser being generous
// and an operator undoing our own mistake are four different things with four
// different windows and four different answers to "who eats the cost". A single
// "refund" button with a single window models none of them, so the reason is a
// stored field and every rule reads from it.
//
// Evaluate is PURE — no clock of its own, no repository, no provider. It is the
// one place a window is computed, so the checkout page that promises a deadline,
// the order page that enables a button and the endpoint that enforces it can
// never disagree about what the policy says.
package refund

import (
	"errors"
	"strings"
	"time"
)

// Reason is the grounds for a refund. Stored, never inferred.
type Reason string

const (
	// ReasonBuyerWithdrawal is CDC art. 49: seven days to change your mind
	// about something bought outside a shop. It is a right, not a request,
	// which is why a request on these grounds inside the window is approved by
	// the policy rather than by a person.
	ReasonBuyerWithdrawal Reason = "buyer_withdrawal"
	// ReasonEventCancelled is the show not happening. Unconditional, whatever
	// the window says, and never declinable.
	ReasonEventCancelled Reason = "event_cancelled"
	// ReasonOrganiserGoodwill is the organiser choosing to refund somebody the
	// policy would not have. Their call, their cost.
	ReasonOrganiserGoodwill Reason = "organiser_goodwill"
	// ReasonOperator is ours: a refund_required order, a duplicate, a fraud
	// reversal. Our timing created it, so it is never billed to the organiser.
	ReasonOperator Reason = "operator"
)

func (r Reason) Valid() bool {
	switch r {
	case ReasonBuyerWithdrawal, ReasonEventCancelled, ReasonOrganiserGoodwill, ReasonOperator:
		return true
	default:
		return false
	}
}

// BuyerMayClaim reports whether a buyer is allowed to ASK on these grounds.
//
// A buyer may withdraw, and may point out that an event was cancelled. They may
// not file a request as the organiser's goodwill or as an operator action,
// which are the two reasons that skip the window: without this check the
// window would be advisory, bypassed by anybody who could set a JSON field.
func (r Reason) BuyerMayClaim() bool {
	return r == ReasonBuyerWithdrawal || r == ReasonEventCancelled
}

// AutoApproves reports whether an allowed request on these grounds needs a
// human at all.
//
// Withdrawal inside the window and a cancelled event are both refunds the
// organiser has no standing to refuse — one is a statutory right, the other is
// a service that will not be delivered. Queueing them for approval would be
// offering a decision that cannot lawfully go the other way, which is worse
// than not offering it: it invites a refusal that then has to be reversed.
//
// Everything else lands pending for a person.
func (r Reason) AutoApproves() bool {
	return r == ReasonBuyerWithdrawal || r == ReasonEventCancelled
}

var (
	ErrNotFound = errors.New("refund request not found")
	// ErrAlreadyOpen is a second request on an order that already has one in
	// flight. Enforced by a partial unique index as well, because a buyer
	// double-tapping "cancelar" is a race a read-then-write loses.
	ErrAlreadyOpen     = errors.New("this order already has a refund request in progress")
	ErrInvalidReason   = errors.New("refund reason is not one of the supported grounds")
	ErrReasonNotYours  = errors.New("this refund reason cannot be claimed by a buyer")
	ErrNotDecidable    = errors.New("this refund request has already been decided")
	ErrInvalidAmount   = errors.New("refund amount must be positive")
	ErrPolicyUnknown   = errors.New("refund policy version is unknown")
	ErrNotRefundable   = errors.New("this order is not in a state that can be refunded")
	ErrWindowClosed    = errors.New("the cancellation window for this order has closed")
	ErrTooCloseToEvent = errors.New("this order can no longer be cancelled this close to the event")
)

// RefusalCode names WHY a refund is not allowed, so the screen can say it.
//
// A bool would lose exactly the thing the buyer needs: "you had until the 12th"
// and "this show is in six hours" are different sentences, and a disabled button
// with no explanation is the most common way a self-service flow becomes a
// support queue.
type RefusalCode string

const (
	RefusalNone RefusalCode = ""
	// RefusalWindowClosed is past the withdrawal days.
	RefusalWindowClosed RefusalCode = "window_closed"
	// RefusalTooCloseToEvent is inside the cutoff before the doors.
	RefusalTooCloseToEvent RefusalCode = "too_close_to_event"
	// RefusalNotPaid is an order no money was taken for. Cancelling an unpaid
	// hold is a different action and the order page offers that one instead.
	RefusalNotPaid         RefusalCode = "not_paid"
	RefusalAlreadyRefunded RefusalCode = "already_refunded"
	// RefusalEventPassed is a show that already happened.
	RefusalEventPassed RefusalCode = "event_passed"
	// RefusalRequestOpen is a request already in flight on this order.
	RefusalRequestOpen RefusalCode = "request_open"
)

// Policy is the rule set, frozen onto an order at checkout.
//
// Frozen for the same reason the tier title and the unit price are copied onto
// an order item: a policy tightened on Tuesday must not have been tightened for
// somebody who bought on Monday. Versions live in code rather than in a table
// because there is one policy for the whole box office today; an organiser-set
// policy becomes a row whose id replaces the version, and every caller below is
// already asking the same question through the same function.
type Policy struct {
	Version int
	// WithdrawalDays is the CDC window, in calendar days from purchase. Seven
	// is the statutory floor and Validate refuses less, because a configuration
	// that is illegal is a configuration that must not boot.
	WithdrawalDays int
	// CutoffBeforeEvent stops a cancellation landing on the day of the show.
	// The window exists so a buyer can reconsider, not so they can hold a seat
	// until the doors and hand it back.
	CutoffBeforeEvent time.Duration
	// RefundsFees is whether the service fee comes back too.
	//
	// It is always true, and it is a field rather than a constant because the
	// CODE has to be able to express both — a future ruling, a different
	// jurisdiction — not because the product offers the choice. Procon-SP's
	// position and recent STJ decisions are that keeping the convenience fee on
	// a cancellation is abusive and restitution must be integral. See
	// docs/REFUNDS_AND_PAYOUTS.md.
	RefundsFees bool
}

// StatutoryWithdrawalDays is CDC art. 49's seven days. A policy may be more
// generous and may never be less.
const StatutoryWithdrawalDays = 7

// CurrentPolicyVersion is what a new order freezes.
const CurrentPolicyVersion = 1

// policies is every version that has ever been live, kept forever.
//
// An old version is never deleted or edited: orders point at it, and the whole
// point of freezing is that looking one up years later still answers what that
// buyer was actually promised.
var policies = map[int]Policy{
	1: {
		Version:           1,
		WithdrawalDays:    StatutoryWithdrawalDays,
		CutoffBeforeEvent: 48 * time.Hour,
		RefundsFees:       true,
	},
}

// PolicyFor returns the frozen policy an order was bought under.
func PolicyFor(version int) (Policy, error) {
	found, ok := policies[version]
	if !ok {
		return Policy{}, ErrPolicyUnknown
	}
	return found, nil
}

// CurrentPolicy is what checkout freezes onto a new order.
func CurrentPolicy() Policy {
	found, err := PolicyFor(CurrentPolicyVersion)
	if err != nil {
		// Unreachable: the constant and the map are edited together, and the
		// test below fails if they ever are not.
		panic("refund: CurrentPolicyVersion has no policy")
	}
	return found
}

// Validate refuses a policy that promises less than the law.
func (p Policy) Validate() error {
	if p.WithdrawalDays < StatutoryWithdrawalDays {
		return errors.New("refund policy cannot offer fewer than the statutory 7 days")
	}
	if p.CutoffBeforeEvent < 0 {
		return errors.New("refund cutoff before the event cannot be negative")
	}
	if !p.RefundsFees {
		return errors.New("refund policy must return the service fee; retaining it is abusive under Procon-SP and STJ")
	}
	return nil
}

// Order is what Evaluate needs to know about a purchase.
//
// A narrow struct rather than the order entity, and that is deliberate: this
// package must not import domain/order, because domain/order will eventually
// want to ask this package a question and an import cycle is the thanks it
// would get. It also keeps Evaluate honest — every input to a window is named
// here, so nothing can quietly start depending on a field nobody declared.
type Order struct {
	ID string
	// Paid is whether money was actually taken. Only a paid order can be
	// refunded; an unpaid hold is cancelled, which is a different action.
	Paid bool
	// AlreadyRefunded short-circuits everything.
	AlreadyRefunded bool
	// PurchasedAt is when the money landed, which is when the CDC clock starts.
	// Not when the order was created: a basket opened on Monday and paid on
	// Thursday withdrew from Thursday.
	PurchasedAt time.Time
	// PolicyVersion is what was frozen at checkout.
	PolicyVersion int
	// HasOpenRequest is whether a request is already in flight.
	HasOpenRequest bool
}

// EventTiming is when the show is. Its own type for the same reason Order is.
type EventTiming struct {
	// StartsAt is the door time. Zero means unknown, which is treated as "no
	// cutoff applies" rather than as "refuse": an order whose event row has
	// gone missing must not trap a buyer's money behind a lookup failure.
	StartsAt time.Time
	// Cancelled is the event being called off, which overrides every window.
	Cancelled bool
}

// Decision is what Evaluate answers.
type Decision struct {
	Allowed bool
	// Until is when a currently-allowed withdrawal stops being allowed.
	//
	// This is the date rendered at checkout and on the order page, computed by
	// the same function that enforces it, so the promise and the enforcement
	// cannot drift. Zero when the question has no deadline — a cancelled event
	// is refundable with no clock on it.
	Until   time.Time
	Refusal RefusalCode
}

// Evaluate decides whether `reason` may refund `order` right now.
//
// Pure. The clock is a parameter, so a test can stand at any instant and the
// screen, the API and the job all pass the same one.
func Evaluate(policy Policy, order Order, event EventTiming, reason Reason, now time.Time) Decision {
	// The two facts that refuse every reason, checked first because no window
	// makes them false.
	if order.AlreadyRefunded {
		return Decision{Refusal: RefusalAlreadyRefunded}
	}
	if !order.Paid {
		return Decision{Refusal: RefusalNotPaid}
	}

	// A cancelled event is refundable unconditionally: no window, no cutoff,
	// no fee kept. Checked before the open-request guard as well, so that the
	// answer to "is this refundable" stays true while one is in flight; the
	// guard below is about opening a SECOND request, and the fan-out for a
	// cancelled event is not a buyer pressing a button.
	if event.Cancelled || reason == ReasonEventCancelled {
		return Decision{Allowed: true}
	}

	if order.HasOpenRequest {
		return Decision{Refusal: RefusalRequestOpen}
	}

	// An operator or the organiser may refund whenever they like. Their
	// authority is checked by the use case; the policy imposes no window on
	// them, which is the whole difference between a right and a favour.
	if reason == ReasonOperator || reason == ReasonOrganiserGoodwill {
		return Decision{Allowed: true}
	}

	// From here it is a buyer withdrawal, and both clocks apply.
	timestamp := now.UTC()
	windowCloses := order.PurchasedAt.UTC().AddDate(0, 0, policy.WithdrawalDays)
	deadline := windowCloses

	if !event.StartsAt.IsZero() {
		doors := event.StartsAt.UTC()
		if !timestamp.Before(doors) {
			return Decision{Refusal: RefusalEventPassed}
		}
		// The cutoff only ever brings the deadline FORWARD. A show three months
		// out does not extend a seven-day window.
		if cutoff := doors.Add(-policy.CutoffBeforeEvent); cutoff.Before(deadline) {
			deadline = cutoff
		}
	}

	if !timestamp.Before(deadline) {
		// Which clock ran out decides the sentence the buyer reads.
		if deadline.Equal(windowCloses) {
			return Decision{Until: deadline, Refusal: RefusalWindowClosed}
		}
		return Decision{Until: deadline, Refusal: RefusalTooCloseToEvent}
	}
	return Decision{Allowed: true, Until: deadline}
}

// Status is where a request stands.
//
// Deliberately three values and not six. Whether the money has actually gone
// back is the ORDER's status, written by the settlement path that reads the
// charge back from the provider, and duplicating it here as "completed" would
// create a second writer for one fact — the failure mode being a request that
// says completed for an order that says paid, with nothing to say which is
// right. The API reports both, and they cannot disagree because only one of
// them is stored.
type Status string

const (
	// StatusPending is waiting on a person.
	StatusPending Status = "pending"
	// StatusApproved has been sent to the money path. The refund job is
	// durable and retried; this status means "we owe this", not "it has left".
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusApproved, StatusRejected:
		return true
	default:
		return false
	}
}

// Open reports whether a request still occupies its order.
func (s Status) Open() bool { return s == StatusPending || s == StatusApproved }

// MaxNoteRunes bounds what a buyer or an organiser may type. Long enough for a
// real explanation, short enough that the column is not a place to paste a log.
const MaxNoteRunes = 1_000

// Request is a refund in flight, as its own aggregate.
//
// Its own table rather than a column on the order, because "who asked, on what
// grounds, decided by whom and when" is a record that has to outlive whatever
// the order ends up as — including a request that was rejected, which leaves no
// trace on the order at all and is exactly the case somebody will ask about.
type Request struct {
	ID      string
	OrderID string
	EventID string
	// BuyerID is the account the order belongs to, copied so the buyer's own
	// listing does not have to join orders.
	BuyerID string
	Reason  Reason
	Status  Status
	// AmountCents is what the buyer gets back, fees included when the policy
	// says so. Frozen at request time from the order, so a later change to the
	// order cannot silently change what was approved.
	AmountCents int64
	// FeeCents is the box office's share of that amount, broken out because the
	// payout ledger will need it and because "did we give our fee back" is a
	// question support will be asked.
	FeeCents int64
	// RequestedBy is the account that asked. Empty for a system-raised request.
	RequestedBy string
	Note        string
	// DecidedBy is empty when the policy approved it rather than a person. That
	// emptiness is the audit trail's way of saying "no human was involved", and
	// it is why this is not defaulted to the requester.
	DecidedBy    string
	DecisionNote string
	DecidedAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AutoApproved reports whether the policy decided this rather than a person.
func (r *Request) AutoApproved() bool {
	return r.Status == StatusApproved && r.DecidedBy == ""
}

// Draft is what a caller supplies to open a request.
type Draft struct {
	OrderID     string
	EventID     string
	BuyerID     string
	Reason      Reason
	AmountCents int64
	FeeCents    int64
	RequestedBy string
	Note        string
}

// New opens a request in the state the grounds imply.
//
// approved reports whether it came out already decided, which is what tells the
// caller to enqueue the money movement in the same transaction.
func New(id string, draft Draft, now time.Time) (*Request, error) {
	if !draft.Reason.Valid() {
		return nil, ErrInvalidReason
	}
	if draft.AmountCents <= 0 {
		return nil, ErrInvalidAmount
	}
	note := strings.TrimSpace(draft.Note)
	if len([]rune(note)) > MaxNoteRunes {
		note = string([]rune(note)[:MaxNoteRunes])
	}

	timestamp := now.UTC()
	request := &Request{
		ID:          id,
		OrderID:     strings.TrimSpace(draft.OrderID),
		EventID:     strings.TrimSpace(draft.EventID),
		BuyerID:     strings.TrimSpace(draft.BuyerID),
		Reason:      draft.Reason,
		Status:      StatusPending,
		AmountCents: draft.AmountCents,
		FeeCents:    draft.FeeCents,
		RequestedBy: strings.TrimSpace(draft.RequestedBy),
		Note:        note,
		CreatedAt:   timestamp,
		UpdatedAt:   timestamp,
	}
	if draft.Reason.AutoApproves() {
		request.Status = StatusApproved
		request.DecidedAt = &timestamp
		// DecidedBy stays empty on purpose: nobody decided this, the policy
		// did.
	}
	return request, nil
}

// Decide records a person's answer.
//
// Not idempotent in the way an order transition is, and deliberately so: a
// second decision on a decided request is a second person disagreeing with the
// first, and swallowing it would lose that. The caller gets an error and the
// screen says who already decided.
func (r *Request) Decide(status Status, deciderID, note string, now time.Time) error {
	if status != StatusApproved && status != StatusRejected {
		return ErrNotDecidable
	}
	if r.Status != StatusPending {
		return ErrNotDecidable
	}
	trimmed := strings.TrimSpace(note)
	if len([]rune(trimmed)) > MaxNoteRunes {
		trimmed = string([]rune(trimmed)[:MaxNoteRunes])
	}

	timestamp := now.UTC()
	r.Status = status
	r.DecidedBy = strings.TrimSpace(deciderID)
	r.DecisionNote = trimmed
	r.DecidedAt = &timestamp
	r.UpdatedAt = timestamp
	return nil
}
