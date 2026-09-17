// Package refund is the application rules around giving money back: who may
// ask, what the policy answers, and who has to agree before it moves.
//
// The domain decides WHETHER (domain/refund.Evaluate, pure). This package
// decides WHO IS ASKING, loads what the domain needs to answer, writes the
// record and hands the money movement to the existing durable refund job. The
// split matters: every window in the product is computed by one pure function,
// so the button on the order page, the deadline promised at checkout and the
// refusal from this endpoint can never disagree.
//
// Two paths reach the same job:
//
//   - A buyer withdrawing inside the statutory window, or claiming a cancelled
//     event, is APPROVED BY THE POLICY. Queueing those for a human would be
//     offering a decision that cannot lawfully go the other way.
//   - Everything else lands pending, and an organiser or an operator decides.
//
// Both leave a row saying who asked, on what grounds, who decided, and when —
// including the rejections, which leave no trace on the order at all and are
// exactly the case support will be asked about.
package refund

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"crypto/rand"
	"encoding/hex"

	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	orderdomain "vozkot/domain/order"
	"vozkot/domain/queue"
	domain "vozkot/domain/refund"
	"vozkot/domain/uow"
	queueUsecase "vozkot/usecases/queue"
)

// ErrForbidden is a caller acting on somebody else's order or event.
//
// Wraps the one sentinel the whole system shares, so transport keeps mapping a
// single error to 403 while the message still says what was refused.
var ErrForbidden = fmt.Errorf("%w: this refund belongs to another account", authdomain.ErrForbidden)

// Money schedules the actual movement. It is satisfied by usecases/payment.
//
// An interface rather than the concrete service because this package does not
// need — and must not acquire — the ability to talk to a payment provider. All
// it may do is put the work on the same durable queue the rest of the money
// path already uses.
type Money interface {
	EnqueueRefund(ctx context.Context, jobs queue.Queue, item *orderdomain.Order) (*queue.Job, error)
}

type Service struct {
	unit       uow.Runner
	requests   domain.Repository
	orders     orderdomain.Repository
	events     eventdomain.Repository
	money      Money
	dispatcher *queueUsecase.Dispatcher
	now        func() time.Time
	newID      func() string
}

func NewService(
	unit uow.Runner,
	requests domain.Repository,
	orders orderdomain.Repository,
	events eventdomain.Repository,
	money Money,
	dispatcher *queueUsecase.Dispatcher,
) *Service {
	return &Service{
		unit:       unit,
		requests:   requests,
		orders:     orders,
		events:     events,
		money:      money,
		dispatcher: dispatcher,
		now:        time.Now,
		newID:      randomID,
	}
}

// Eligibility is what the order page asks before it draws a button.
type Eligibility struct {
	Decision domain.Decision
	// AmountCents is what the buyer would get back, so the confirmation dialog
	// can name it rather than saying "a refund".
	AmountCents int64
	FeeCents    int64
	// OrganiserCents is the share of that refund which comes out of the
	// organiser's revenue: the face value, with our commission taken off.
	//
	// It is the only one of these three an organiser is shown. See
	// SeesPlatformShare.
	OrganiserCents int64
	// RefundsFees is what the policy promises about the service fee, rendered
	// beside the amount because "R$ 110,00, taxa incluída" is the sentence that
	// stops the support ticket.
	RefundsFees bool
	// ShowsPlatformShare is whether the CALLER may be told AmountCents and
	// FeeCents at all.
	//
	// Decided here rather than at the transport edge, and carried on the value
	// rather than recomputed, because this struct reaches a JSON encoder and a
	// rule re-derived next to an encoder is a rule the next endpoint forgets.
	ShowsPlatformShare bool
}

// Evaluate answers whether a refund is allowed right now, and why not.
//
// Called by the order page to decide whether the button is live, and by Request
// to decide whether to accept. One function, two callers, no drift.
func (s *Service) Evaluate(
	ctx context.Context,
	orderID string,
	actor authdomain.Actor,
	reason domain.Reason,
) (Eligibility, error) {
	item, err := s.orders.GetByID(ctx, strings.TrimSpace(orderID))
	if err != nil {
		return Eligibility{}, err
	}
	if err := s.mayAct(ctx, item, actor, reason); err != nil {
		return Eligibility{}, err
	}
	eligibility, err := s.evaluate(ctx, item, reason)
	if err != nil {
		return Eligibility{}, err
	}
	eligibility.ShowsPlatformShare = SeesPlatformShare(actor, item.BuyerID)
	return eligibility, nil
}

// evaluate is the authorised half, shared by Evaluate and Request so the check
// and the enforcement read the same state.
func (s *Service) evaluate(
	ctx context.Context,
	item *orderdomain.Order,
	reason domain.Reason,
) (Eligibility, error) {
	policy, err := domain.PolicyFor(item.RefundPolicyVersion)
	if err != nil {
		// An order older than the policy register. Falling back to the current
		// policy would silently apply today's rules to yesterday's purchase,
		// which is the exact thing freezing exists to prevent, so the oldest
		// known policy is used instead and the buyer gets the terms that were
		// live when the register began.
		policy = oldestPolicy()
	}

	open, err := s.requests.FindOpenByOrder(ctx, item.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return Eligibility{}, err
	}

	decision := domain.Evaluate(policy, s.snapshot(item, open != nil), s.timingFor(ctx, item.EventID), reason, s.now())
	amount, fee := refundable(item, policy)
	return Eligibility{
		Decision:       decision,
		AmountCents:    amount,
		FeeCents:       fee,
		OrganiserCents: amount - fee,
		RefundsFees:    policy.RefundsFees,
	}, nil
}

// RequestInput is somebody asking for money back.
type RequestInput struct {
	OrderID string
	// Actor is the authenticated caller, never taken from the
	// body: the difference between a buyer and an organiser is the difference
	// between a window and no window.
	Actor  authdomain.Actor
	Reason domain.Reason
	Note   string
}

// Request opens a refund request, approving it on the spot when the grounds are
// ones nobody may refuse.
//
// The write and the money are ONE transaction. An approval committed without
// its job is a buyer told they are refunded who never is; a job committed
// without its approval is money leaving with no record of who allowed it.
func (s *Service) Request(ctx context.Context, input RequestInput) (*domain.Request, error) {
	if !input.Reason.Valid() {
		return nil, domain.ErrInvalidReason
	}

	item, err := s.orders.GetByID(ctx, strings.TrimSpace(input.OrderID))
	if err != nil {
		return nil, err
	}
	if err := s.mayAct(ctx, item, input.Actor, input.Reason); err != nil {
		return nil, err
	}

	eligibility, err := s.evaluate(ctx, item, input.Reason)
	if err != nil {
		return nil, err
	}
	if !eligibility.Decision.Allowed {
		return nil, refusal(eligibility.Decision.Refusal)
	}

	var created *domain.Request
	var announce *queue.Job

	err = s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		request, err := domain.New(s.newID(), domain.Draft{
			OrderID:     item.ID,
			EventID:     item.EventID,
			BuyerID:     item.BuyerID,
			Reason:      input.Reason,
			AmountCents: eligibility.AmountCents,
			FeeCents:    eligibility.FeeCents,
			RequestedBy: input.Actor.ID,
			Note:        input.Note,
		}, s.now())
		if err != nil {
			return err
		}
		// The partial unique index decides this, not a prior read: a buyer
		// double-tapping "cancelar" is the race a read-then-write loses, and
		// losing it means two refunds for one order.
		if err := repositories.Refunds().Create(ctx, request); err != nil {
			return err
		}
		created = request

		if request.Status != domain.StatusApproved {
			// Waiting on a person. No money is scheduled yet.
			return nil
		}
		job, err := s.money.EnqueueRefund(ctx, repositories.Jobs(), item)
		if err != nil {
			return err
		}
		announce = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Announced only after the commit, like every other job in this system:
	// publishing inside the transaction tells a consumer about work that may
	// still roll back.
	s.dispatcher.Dispatch(ctx, announce)
	return created, nil
}

// DecideInput is an organiser or an operator answering a pending request.
type DecideInput struct {
	RequestID string
	Actor     authdomain.Actor
	// Approve is the answer. Rejecting is as much a decision as approving and
	// is recorded the same way.
	Approve bool
	Note    string
}

// Decide records the answer and, on approval, schedules the money.
func (s *Service) Decide(ctx context.Context, input DecideInput) (*domain.Request, error) {
	var decided *domain.Request
	var announce *queue.Job

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		// Locked for the rest of the transaction, so two people pressing
		// approve and reject at the same moment take turns and the second sees
		// the first's answer rather than overwriting it.
		request, err := repositories.Refunds().GetByIDForUpdate(ctx, strings.TrimSpace(input.RequestID))
		if err != nil {
			return err
		}
		item, err := repositories.Orders().GetByIDForUpdate(ctx, request.OrderID)
		if err != nil {
			return err
		}
		if err := s.mayDecide(ctx, item, input.Actor); err != nil {
			return err
		}

		status := domain.StatusRejected
		if input.Approve {
			status = domain.StatusApproved
		}
		if err := request.Decide(status, input.Actor.ID, input.Note, s.now()); err != nil {
			return err
		}
		if err := repositories.Refunds().Update(ctx, request); err != nil {
			return err
		}
		decided = request

		if !input.Approve {
			return nil
		}
		job, err := s.money.EnqueueRefund(ctx, repositories.Jobs(), item)
		if err != nil {
			return err
		}
		announce = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.dispatcher.Dispatch(ctx, announce)
	return decided, nil
}

// Get returns one request, scoped to somebody entitled to see it.
func (s *Service) Get(ctx context.Context, id string, actor authdomain.Actor) (*domain.Request, error) {
	request, err := s.requests.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	// The same people who may see our side of the money are the ones for whom
	// this request is their own; anybody else has to own the event.
	if SeesPlatformShare(actor, request.BuyerID, request.RequestedBy) {
		return request, nil
	}
	if err := s.ownsEvent(ctx, request.EventID, actor); err != nil {
		return nil, err
	}
	return request, nil
}

// List is the organiser's inbox and the buyer's own history, depending on who
// asks. The scoping is applied by the caller at the transport edge, where the
// identity is known, and re-checked here for the event case.
func (s *Service) List(ctx context.Context, filter domain.Filter, actor authdomain.Actor) (domain.Page, error) {
	if !actor.IsAdmin() && filter.EventID != "" {
		if err := s.ownsEvent(ctx, filter.EventID, actor); err != nil {
			return domain.Page{}, err
		}
	}
	return s.requests.List(ctx, filter)
}

// OpenByOrders reports which of a page of orders already have a request in
// flight, so an orders listing can render its buttons without asking once per
// row.
func (s *Service) OpenByOrders(ctx context.Context, orderIDs []string) (map[string]domain.Request, error) {
	return s.requests.OpenByOrders(ctx, orderIDs)
}

// SeesPlatformShare reports whether a caller may be told the GROSS refund and
// how much of it is our commission.
//
// The buyer may: it is their money coming back, and the Terms of Service
// promise them the service fee returns with it, so naming the whole figure is
// what stops a dispute. An operator may, because supporting the sale requires
// it. The ORGANISER may not. What leaves their revenue on a refund is the face
// value they priced; the rest of the figure is our commission returning to the
// buyer, which is a matter between us and them.
//
// owners are the account ids that make this the caller's own money — the
// order's buyer, or the person who filed the request. Empty ones never match,
// so a door sale with no account behind it does not make every caller an owner.
func SeesPlatformShare(actor authdomain.Actor, owners ...string) bool {
	if actor.IsAdmin() {
		return true
	}
	for _, owner := range owners {
		if actor.Owns(owner) {
			return true
		}
	}
	return false
}

// mayAct refuses a caller asking on grounds that are not theirs to claim.
//
// This is the check that makes the policy window real. Without it, a buyer who
// can set a JSON field picks organiser_goodwill, which has no window, and the
// seven days become advisory.
func (s *Service) mayAct(
	ctx context.Context,
	item *orderdomain.Order,
	actor authdomain.Actor,
	reason domain.Reason,
) error {
	if actor.IsAdmin() {
		return nil
	}
	owner := actor.Owns(item.BuyerID)
	if owner {
		if !reason.BuyerMayClaim() {
			return domain.ErrReasonNotYours
		}
		return nil
	}
	// Not the buyer: the only other person with standing is the organiser whose
	// event this is, and they act on their own grounds.
	if err := s.ownsEvent(ctx, item.EventID, actor); err != nil {
		return err
	}
	if reason == domain.ReasonOperator {
		// Ours to claim, not theirs: it is the reason that never bills the
		// organiser, so an organiser must not be able to choose it.
		return domain.ErrReasonNotYours
	}
	return nil
}

// mayDecide refuses anybody but the organiser or an operator.
//
// The buyer is deliberately absent: a request the requester could approve is
// not a request.
func (s *Service) mayDecide(ctx context.Context, item *orderdomain.Order, actor authdomain.Actor) error {
	if actor.IsAdmin() {
		return nil
	}
	return s.ownsEvent(ctx, item.EventID, actor)
}

func (s *Service) ownsEvent(ctx context.Context, eventID string, actor authdomain.Actor) error {
	if strings.TrimSpace(eventID) == "" || !actor.Authenticated() {
		return ErrForbidden
	}
	happening, err := s.events.GetByID(ctx, eventID)
	if err != nil {
		if errors.Is(err, eventdomain.ErrNotFound) {
			return ErrForbidden
		}
		return err
	}
	// The same check the report and the door make, from one place.
	if err := happening.AuthorizeReach(actor); err != nil {
		return err
	}
	return nil
}

// snapshot reduces an order to the facts the policy needs.
func (s *Service) snapshot(item *orderdomain.Order, hasOpenRequest bool) domain.Order {
	purchased := item.CreatedAt
	if item.PaidAt != nil {
		// The CDC clock starts when the money landed, not when the basket was
		// opened: a basket opened on Monday and paid on Thursday withdrew from
		// Thursday.
		purchased = *item.PaidAt
	}
	return domain.Order{
		ID:              item.ID,
		Paid:            item.Status == orderdomain.StatusPaid || item.Status == orderdomain.StatusRefundRequired,
		AlreadyRefunded: item.Status == orderdomain.StatusRefunded,
		PurchasedAt:     purchased,
		PolicyVersion:   item.RefundPolicyVersion,
		HasOpenRequest:  hasOpenRequest,
	}
}

// timingFor reads when the show is, and treats a failure as "unknown" rather
// than as a refusal.
//
// An order whose event row cannot be read must not trap a buyer's money behind
// a lookup failure. Unknown timing applies no cutoff, which is the generous
// direction, and the seven-day window still bounds it.
func (s *Service) timingFor(ctx context.Context, eventID string) domain.EventTiming {
	if strings.TrimSpace(eventID) == "" {
		return domain.EventTiming{}
	}
	happening, err := s.events.GetByID(ctx, eventID)
	if err != nil {
		return domain.EventTiming{}
	}
	return domain.EventTiming{
		StartsAt:  happening.StartsAt,
		Cancelled: happening.Status == eventdomain.StatusCancelled,
	}
}

// refundable is what the buyer gets back and how much of it is our fee.
//
// The whole amount, fee included, because that is what the policy says and what
// Procon-SP and the STJ require: restitution has to be integral. The fee is
// broken out rather than hidden inside the total so support can answer "did we
// give our commission back too" with a number instead of an opinion.
func refundable(item *orderdomain.Order, policy domain.Policy) (amount, fee int64) {
	if policy.RefundsFees {
		return item.TotalCents, item.BuyerFeeCents
	}
	// Unreachable while Validate refuses a policy that keeps the fee, and kept
	// because the code has to be able to express the other world if the law
	// ever changes. The buyer gets the face value; the fee stays with us.
	return item.SubtotalCents, 0
}

// oldestPolicy is the fallback for an order predating the policy register.
func oldestPolicy() domain.Policy {
	oldest, err := domain.PolicyFor(1)
	if err != nil {
		return domain.CurrentPolicy()
	}
	return oldest
}

// refusal turns a policy refusal into an error the transport can map.
func refusal(code domain.RefusalCode) error {
	switch code {
	case domain.RefusalWindowClosed:
		return domain.ErrWindowClosed
	case domain.RefusalTooCloseToEvent, domain.RefusalEventPassed:
		return domain.ErrTooCloseToEvent
	case domain.RefusalRequestOpen:
		return domain.ErrAlreadyOpen
	default:
		return fmt.Errorf("%w (%s)", domain.ErrNotRefundable, code)
	}
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "rfr_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "rfr_" + hex.EncodeToString(buffer)
}
