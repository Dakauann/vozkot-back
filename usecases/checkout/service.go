// Package checkout turns "I want three Pista and one Camarote" into a held
// order waiting for money.
//
// Stock and the order are written in ONE transaction. A reservation without an
// order is inventory nobody can claim; an order without a reservation is a
// promise the box office cannot keep. Committing them together is what makes
// both impossible.
//
// The hold happens in two phases, and that is a deliberate departure from the
// obvious design:
//
//   - RESERVE, the moment a buyer reaches the details form. The tickets come
//     off the shelf immediately, for a short window. Waiting until the form is
//     submitted means every buyer fills in their name, their email and their
//     CPF against stock anyone can still take, and learns at the last keystroke
//     that it is gone.
//   - CONFIRM, when they submit it. The window extends to the full payment
//     window and the charge is requested.
//
// One window for both would have to be the long one, and a long window opened
// by merely visiting a page is how an event gets held hostage by a crawler.
package checkout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	authdomain "vozkot/domain/auth"
	orderdomain "vozkot/domain/order"
	"vozkot/domain/payment"
	"vozkot/domain/pricing"
	"vozkot/domain/queue"
	refunddomain "vozkot/domain/refund"
	seatingdomain "vozkot/domain/seating"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/domain/uow"
	userdomain "vozkot/domain/user"
	queueUsecase "vozkot/usecases/queue"
)

// DefaultHoldFor is how long a buyer has to pay once they have confirmed.
//
// Thirty minutes is a judgement about people, not about a provider: long enough
// to open a banking app, authenticate and find the PIX screen without hurrying,
// short enough that an abandoned order returns its seats while the event is
// still selling.
//
// It used to be dictated by Mercado Pago, which refuses a PIX charge expiring
// sooner. Asaas does not have that floor: it dates a charge to a DAY, which
// means the charge outlives this hold no matter what is chosen here. The
// consequence is that a payment arriving after the hold lapsed is the ORDINARY
// case rather than an edge one: settlement asks for the stock back, takes it if
// it is still there, and owes a refund if it is not. That path is implemented
// and tested; see infra/asaas/gateway.go for why the gap exists at all.
const DefaultHoldFor = 30 * time.Minute

// DefaultCartHoldFor is how long the tickets stay off the shelf while the buyer
// fills in the form.
//
// Ten minutes, which is generous for typing a name and an email and short
// enough that an abandoned basket is back on sale before anyone notices it
// left. It is deliberately NOT the payment window: nothing has been asked of
// the buyer yet, and stock held on the strength of a page view should cost the
// event as little as possible.
const DefaultCartHoldFor = 10 * time.Minute

// Settings are the box office's policies for a purchase: how long stock is
// held, how much of it one account may hold, and what the buyer is charged on
// top of the tier price.
//
// A struct rather than five more positional parameters. The constructor was at
// seven and every one of them was a duration, an int or a struct of ints, which
// is the shape where swapping two arguments compiles cleanly and silently
// halves a hold window. The zero value is a working configuration: default
// windows, no caps, no fee.
type Settings struct {
	HoldFor     time.Duration
	CartHoldFor time.Duration
	// HoldLimits caps what one account may keep reserved and unpaid at once.
	// The zero value caps nothing, which is what the load harness wants.
	HoldLimits orderdomain.HoldLimits
	// Fee is the service charge added on top of the face value. The zero value
	// charges nothing, so a deployment that has not configured one sells at the
	// tier price exactly as it did before fees existed.
	Fee pricing.Fee
}

func (s Settings) normalize() Settings {
	if s.HoldFor <= 0 {
		s.HoldFor = DefaultHoldFor
	}
	if s.CartHoldFor <= 0 {
		s.CartHoldFor = DefaultCartHoldFor
	}
	if s.CartHoldFor > s.HoldFor {
		// A cart window longer than the payment window would mean confirming an
		// order SHORTENED its hold, which is the opposite of what confirming
		// is for. Clamping is better than refusing to start over a
		// misconfiguration with an obvious right answer.
		s.CartHoldFor = s.HoldFor
	}
	return s
}

type Service struct {
	unit uow.Runner
	// dispatcher announces committed jobs to the broker. Nil is supported and
	// means the poller is the only delivery path.
	dispatcher *queueUsecase.Dispatcher
	orders     orderdomain.Repository
	tickets    ticketdomain.Repository
	// buyers is read for the optional demographics frozen onto an order. Nil is
	// supported and means every order is created with none of them, which is
	// what the load harness and any deployment without an encryption keyring
	// get: a missing audience report, never a missing sale.
	buyers   userdomain.Repository
	settings Settings
	// voider cancels the provider charge of an order whose hold lapsed. Nil
	// leaves the code payable until the provider's own due date.
	voider voider
	now    func() time.Time
	newID  func() string
}

func NewService(
	unit uow.Runner,
	orders orderdomain.Repository,
	tickets ticketdomain.Repository,
	buyers userdomain.Repository,
	dispatcher *queueUsecase.Dispatcher,
	settings Settings,
) *Service {
	return &Service{
		unit:       unit,
		dispatcher: dispatcher,
		orders:     orders,
		tickets:    tickets,
		buyers:     buyers,
		settings:   settings.normalize(),
		now:        time.Now,
		newID:      randomID,
	}
}

// Fee is the commission this box office is charging, so a catalogue endpoint
// can quote a buyer the real price before they reach the checkout.
//
// Exposed from here rather than read from configuration a second time: the
// number the event page promises and the number the order charges have to be
// the same number, and the only way to guarantee that is for there to be one.
func (s *Service) Fee() pricing.Fee { return s.settings.Fee }

// voider is the slice of usecases/payment this file uses.
//
// Declared at the consumer, like every other cross-use-case seam here, so
// checkout keeps knowing nothing about payment providers.
type voider interface {
	EnqueueCancelCharge(ctx context.Context, jobs queue.Queue, item *orderdomain.Order) (*queue.Job, error)
}

// WithChargeVoider makes an expiring hold take its payment code with it.
//
// THE WINDOW THIS CLOSES: a hold is thirty minutes and a provider dates a
// charge to a DAY, so releasing the stock left a live, payable PIX code out in
// the world for another twenty-odd hours. Whoever paid it sent real money for
// seats that were already back on sale, and the box office could only take the
// money and hand it straight back, minus the provider's fee.
//
// Optional: without it expiry behaves exactly as it did, and settlement still
// handles a late payment by re-taking the stock or owing a refund.
func (s *Service) WithChargeVoider(charges voider) *Service {
	s.voider = charges
	return s
}

// StartInput is one purchase attempt: one event, one or more of its tiers.
type StartInput struct {
	Items         []orderdomain.DraftItem
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string
	Method        payment.Method
	// IdempotencyKey is the client's retry token. The HTTP layer enforces the
	// replay contract; the order keeps the key so an order can always be traced
	// back to the request that made it.
	IdempotencyKey string
	// Confirm collapses both phases into one call: reserve and immediately ask
	// for the charge. It exists for callers that have the buyer's details
	// already, an operator selling at the door, and the load harness, and not
	// for the browser flow, which always confirms as its own step.
	Confirm bool
}

// Start reserves stock for every line and opens an order.
//
// The charge itself is NOT created here, and on the browser path it is not
// created by this call at all. Asking a payment provider for a PIX code is a
// round trip to a third party that is occasionally slow and occasionally down,
// and a buyer holding a reservation should not wait on it, nor should the
// reservation be lost because the provider timed out. The job enqueued in this
// same transaction owns that call, which is the outbox pattern: the work cannot
// be scheduled unless the order it refers to was committed, and cannot be lost
// if it was.
func (s *Service) Start(ctx context.Context, input StartInput) (*orderdomain.Order, error) {
	requested, err := orderdomain.NormalizeItems(input.Items)
	if err != nil {
		return nil, err
	}

	// Read OUTSIDE the transaction, and before it. The demographics are a
	// snapshot for a report, not something stock or money depends on, so
	// holding a transaction open across this read would add a query to the
	// hottest path in the system to buy consistency nothing needs.
	audience := s.audienceFor(ctx, input.BuyerID)

	var created *orderdomain.Order
	var announce []*queue.Job

	// Minted before the transaction, not inside it, because a claimed SEAT
	// names the order holding it and the claim happens before the order row is
	// written. There is no foreign key between the two for exactly this reason:
	// the seat is inventory and the order is a record of a sale, and the
	// inventory has to be takeable first.
	orderID := s.newID()
	now := s.now()
	holdFor := s.settings.CartHoldFor
	if input.Confirm {
		holdFor = s.settings.HoldFor
	}
	holdExpiresAt := now.Add(holdFor)

	// Which chairs each tier was asked for, keyed by tier, from the normalised
	// draft. Read here so the reservation loop below does not have to carry the
	// draft around beside the priced lines.
	requestedSeats := make(map[string][]string, len(requested))
	for _, wanted := range requested {
		if wanted.Seated() {
			requestedSeats[wanted.TicketID] = wanted.SeatIDs
		}
	}

	err = s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		// Read every tier first, before anything is reserved. The prices, the
		// titles and the event all come from these rows and never from the
		// request: a client that names its own price names nothing.
		lines := make([]orderdomain.Item, 0, len(requested))
		eventID := ""
		currency := ""
		for _, wanted := range requested {
			tier, err := repositories.Tickets().GetByID(ctx, wanted.TicketID)
			if err != nil {
				return err
			}
			if eventID == "" {
				eventID = tier.EventID
			} else if tier.EventID != eventID {
				// One order is one night. Two events in a basket would share a
				// hold window, a PIX code and a receipt that could only name
				// one venue.
				return orderdomain.ErrMultipleEvents
			}
			if currency == "" {
				currency = tier.Currency
			}
			// Advisory, for a clear error message. The authority is the
			// conditional update below, which is the only check two concurrent
			// buyers cannot both pass.
			if err := tier.CanReserve(wanted.Quantity); err != nil {
				return err
			}
			lines = append(lines, orderdomain.Item{
				ID:             s.newItemID(),
				TicketID:       tier.ID,
				TicketTitle:    tier.Title,
				Quantity:       wanted.Quantity,
				UnitPriceCents: tier.PriceCents,
			})
		}

		// Hold the manifest stable through the reservation. Configuration takes an
		// exclusive lock, so an area cannot be reassigned during checkout.
		manifest, err := repositories.Seats().SeatingOf(ctx, eventID)
		if err != nil {
			return err
		}
		if manifest != nil {
			for _, area := range manifest.Areas {
				for index := range lines {
					if lines[index].TicketID == area.TicketID {
						lines[index].TicketTitle = area.Name
					}
				}
			}
		}

		// A mixed event has counted tiers and tiers that require named chairs.
		// Determine that from inventory, never from the presence of seat IDs in
		// the request: omitting them must not bypass reservation of a chair.
		seatCounts, err := repositories.Seats().CountsByEvent(ctx, eventID)
		if err != nil {
			return err
		}
		for _, counts := range seatCounts {
			for _, wanted := range requested {
				if wanted.TicketID == counts.TicketID && counts.Total() > 0 && !wanted.Seated() {
					return fmt.Errorf("%w: choose numbered seats for this ticket", orderdomain.ErrInvalidTicket)
				}
			}
		}

		// Before any inventory moves: is this account already sitting on more
		// than it is allowed to? The check is inside the transaction and behind
		// a per-buyer lock, so two simultaneous checkouts cannot both pass it.
		//
		// It runs BEFORE the reservations so the lock order is always
		// buyer-then-tiers. Taking them the other way round in some other path
		// would be the classic deadlock; there is only one path, and this is
		// it.
		if err := s.withinHoldLimits(ctx, repositories, input.BuyerID, lines); err != nil {
			return err
		}

		// In the order NormalizeItems sorted them into, which is what keeps two
		// orders over the same two tiers from deadlocking against each other.
		//
		// Both kinds of stock move here. A counted line moves the tier's
		// counter and nothing else, exactly as it always has. A SEATED line
		// claims its named chairs AND moves the same counter, because the
		// counter is a projection the catalogue, the tier picker and the
		// sold-out transition all read, and because the CHECK constraint on it
		// is the one guard the application cannot bypass.
		seatsByTier := make(map[string][]seatingdomain.EventSeat, len(lines))
		for _, line := range lines {
			if seats := requestedSeats[line.TicketID]; len(seats) > 0 {
				claimed, err := claimSeats(ctx, repositories, seatingdomain.ClaimRequest{
					EventID:       eventID,
					TicketID:      line.TicketID,
					OrderID:       orderID,
					SeatIDs:       seats,
					HoldExpiresAt: holdExpiresAt,
				})
				if err != nil {
					return err
				}
				seatsByTier[line.TicketID] = claimed
			}
			reserved, err := repositories.Tickets().Reserve(ctx, line.TicketID, line.Quantity)
			if err != nil {
				return err
			}
			if !reserved {
				// Someone took the last ones between the read and the update.
				// This is the race the conditional update exists to lose
				// safely, and losing it rolls the whole transaction back,
				// including every line already reserved above, which is why a
				// partial basket can never be committed.
				return ticketdomain.ErrInsufficientStock
			}
		}

		// One row per chair, now that the claim has told us which chairs and
		// what they are called. Done after the claim rather than before it
		// because the labels are a SNAPSHOT and the only honest source for them
		// is the row that was actually claimed.
		lines = expandSeatedLines(lines, seatsByTier, s.newItemID)

		item, err := orderdomain.New(orderID, orderdomain.Draft{
			EventID:       eventID,
			BuyerID:       input.BuyerID,
			BuyerName:     input.BuyerName,
			BuyerEmail:    input.BuyerEmail,
			BuyerDocument: input.BuyerDocument,
			BuyerGender:   audience.Gender,
			BuyerAgeYears: audience.AgeYears,
			BuyerCity:     audience.City,
			BuyerUF:       audience.UF,
			Items:         lines,
			// The fee is applied HERE, once, from the box office's own
			// configuration. Never from the request: a client that could name
			// the commission could name zero.
			Fee:            s.settings.Fee,
			Currency:       currency,
			Method:         input.Method,
			IdempotencyKey: input.IdempotencyKey,
			// The cancellation rules are frozen onto the order at the moment of
			// purchase, so tightening the policy later cannot tighten it for
			// somebody who already bought.
			RefundPolicyVersion: refunddomain.CurrentPolicyVersion,
		}, holdFor, now)
		if err != nil {
			return err
		}
		if input.Confirm {
			if _, err := item.Confirm(
				input.BuyerName, input.BuyerEmail, input.BuyerDocument,
				input.Method, s.settings.HoldFor, s.now(),
			); err != nil {
				return err
			}
		}
		if err := repositories.Orders().Create(ctx, item); err != nil {
			return err
		}

		if input.Confirm {
			charge, err := s.enqueueCharge(ctx, repositories.Jobs(), item)
			if err != nil {
				return err
			}
			if charge != nil {
				announce = append(announce, charge)
			}
		}
		// The hold's expiry is scheduled as its own job rather than left to the
		// sweep, so stock comes back the moment it is due even on a quiet
		// system. The sweep stays as the safety net.
		if _, err := s.enqueueExpiry(ctx, repositories.Jobs(), item); err != nil {
			return err
		}

		created = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Announced only now: publishing inside the transaction would tell a
	// consumer about an order that might still roll back, and consumers are
	// fast enough to look for it before it exists.
	s.dispatcher.Dispatch(ctx, announce...)
	return created, nil
}

// ConfirmInput is the details step: who is buying, and how they will pay.
type ConfirmInput struct {
	OrderID string
	// BuyerID is the signed-in account. An order may only be confirmed by the
	// account that opened it, checked here rather than only at the edge so no
	// future caller can skip it.
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string
	Method        payment.Method
}

// Confirm accepts the buyer's details, extends the hold to the payment window
// and asks for the charge.
//
// Safe to call twice. The second call saves any corrected details, does not
// extend the window again, and does not create a second charge, the job's
// dedupe key sees to the last of those even if this method's own guard were
// ever removed.
func (s *Service) Confirm(ctx context.Context, input ConfirmInput) (*orderdomain.Order, error) {
	var result *orderdomain.Order
	var chargeJob *queue.Job

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		item, err := repositories.Orders().GetByIDForUpdate(ctx, input.OrderID)
		if err != nil {
			return err
		}
		if item.BuyerID != "" && input.BuyerID != "" && item.BuyerID != input.BuyerID {
			return orderdomain.ErrNotFound
		}
		if item.HoldLapsed(s.now()) {
			// The window closed while the form was open. Say so rather than
			// extending a hold whose stock the sweep is about to return.
			return orderdomain.ErrHoldExpired
		}

		extended, err := item.Confirm(
			input.BuyerName, input.BuyerEmail, input.BuyerDocument,
			input.Method, s.settings.HoldFor, s.now(),
		)
		if err != nil {
			return err
		}
		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		result = item
		if !extended {
			// Already confirmed. The details were saved above; nothing else
			// about this call is new.
			return nil
		}

		charge, err := s.enqueueCharge(ctx, repositories.Jobs(), item)
		if err != nil {
			return err
		}
		chargeJob = charge
		// The hold now ends later than the job scheduled at reserve time. That
		// job will fire, find the hold still standing and do nothing; this one
		// is the one that will actually expire it.
		if _, err := s.enqueueExpiry(ctx, repositories.Jobs(), item); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.dispatcher.Dispatch(ctx, chargeJob)
	return result, nil
}

// audience is the coarse, non-identifying snapshot frozen onto an order.
type audience struct {
	Gender   string
	AgeYears int
	City     string
	UF       string
}

// audienceFor reads the optional demographics off the buyer's profile.
//
// It NEVER fails a checkout, and that is the whole contract. Every field it
// reads is optional, the thing it feeds is a report, and the worst outcome of
// this read going wrong is one order counted as "not informed" in a breakdown.
// Weighed against that, a buyer being told their purchase failed because the
// users table was briefly unavailable is not a trade anybody would make, so a
// failure is logged and the sale proceeds.
//
// An anonymous sale (an operator at the door, the load harness) has no profile
// to read and returns the zero value, which is exactly "not informed".
func (s *Service) audienceFor(ctx context.Context, buyerID string) audience {
	if s.buyers == nil || strings.TrimSpace(buyerID) == "" {
		return audience{}
	}
	account, err := s.buyers.FindByID(ctx, buyerID)
	if err != nil {
		if !errors.Is(err, userdomain.ErrNotFound) {
			log.Printf("checkout: could not read the profile for buyer %s; the order is counted as not informed: %v",
				buyerID, err)
		}
		return audience{}
	}
	profile := account.Profile
	return audience{
		Gender: string(profile.Gender),
		// Computed at the moment of purchase and stored as a number, so the
		// report never has to hold a date of birth and never has to re-age
		// anybody: a buyer who turns 29 next week was 28 when they bought.
		AgeYears: profile.AgeOn(s.now()),
		City:     profile.City,
		UF:       profile.UF,
	}
}

// withinHoldLimits refuses a buyer already holding more unpaid inventory than
// the box office allows.
//
// This is what closes the arithmetic the per-minute rate limit leaves open. At
// thirty checkouts a minute, ten tickets each and a thirty-minute hold, an
// account that merely cycles can keep nine thousand tickets off the shelf with
// no money at risk: the rate limit bounds how fast someone reserves, never how
// much they are sitting on, and it is the second number that empties an event.
func (s *Service) withinHoldLimits(
	ctx context.Context,
	repositories uow.Repositories,
	buyerID string,
	lines []orderdomain.Item,
) error {
	if s.settings.HoldLimits.Unlimited() || buyerID == "" {
		// No limits configured, or a sale with no account behind it: nothing to
		// count against, and no reason to pay for the lock.
		return nil
	}
	tiers := make([]string, 0, len(lines))
	for _, line := range lines {
		tiers = append(tiers, line.TicketID)
	}
	current, err := repositories.Orders().CountOpenHoldsForUpdate(ctx, buyerID, tiers)
	if err != nil {
		return err
	}
	return s.settings.HoldLimits.Allows(current, lines)
}

// Get returns one order to the buyer it belongs to, or to an operator.
func (s *Service) Get(ctx context.Context, actor authdomain.Actor, id string) (*orderdomain.Order, error) {
	return s.owned(ctx, actor, id)
}

// owned loads an order and refuses one belonging to another buyer.
//
// The single authorisation point for reading an order. An order with NO buyer,
// a door sale taken by an operator, is reachable only by an operator:
// MayReach refuses an empty owner rather than treating it as everybody's.
func (s *Service) owned(ctx context.Context, actor authdomain.Actor, id string) (*orderdomain.Order, error) {
	item, err := s.orders.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.MayReach(item.BuyerID) {
		return nil, fmt.Errorf("%w: this order belongs to another buyer", authdomain.ErrForbidden)
	}
	return item, nil
}

// FindByIdempotencyKey returns the order a checkout key produced, if it
// produced one.
//
// It exists for recovery: the idempotency claim is written before the checkout
// and completed after it, so a process killed between the two leaves a real
// order behind a key that looks unstarted. The buyer's retry has to be able to
// find that order and be shown it, rather than reserving a second batch of
// tickets they never asked for.
func (s *Service) FindByIdempotencyKey(ctx context.Context, key string) (*orderdomain.Order, error) {
	return s.orders.FindByIdempotencyKey(ctx, key)
}

// Page is a listing plus its total.
type Page struct {
	Items []orderdomain.Order
	Total int64
}

// List is a buyer's own orders, or the whole box office for an operator.
//
// The narrowing is here and not in the handler: an order carries a name, an
// email and a document, so an unscoped listing is a customer database.
func (s *Service) List(ctx context.Context, actor authdomain.Actor, filter orderdomain.Filter) (Page, error) {
	if !actor.IsAdmin() {
		if !actor.Authenticated() {
			return Page{}, authdomain.ErrUnauthorized
		}
		filter.BuyerID = actor.ID
	}

	items, err := s.orders.List(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	total, err := s.orders.Count(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, Total: total}, nil
}

// Cancel gives up a pending order and returns its stock.
//
// Cancelling an order that is already cancelled succeeds and changes nothing: a
// buyer who taps the button twice has not done anything wrong.
func (s *Service) Cancel(ctx context.Context, id string) (*orderdomain.Order, error) {
	var result *orderdomain.Order

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		item, err := repositories.Orders().GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		heldStock := item.Status.HoldsStock()

		changed, err := item.Apply(orderdomain.StatusCancelled, s.now())
		if err != nil {
			return err
		}
		result = item
		if !changed {
			return nil
		}
		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		if heldStock {
			return releaseAll(ctx, repositories, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ExpireHolds returns the stock of every order whose window has passed.
//
// Claim-then-release, never read-then-release: the repository flips the rows to
// expired in one statement and hands back only what it actually flipped, so two
// sweepers running at once split the work instead of both releasing the same
// tickets.
func (s *Service) ExpireHolds(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	released := 0
	var announce []*queue.Job

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		// Reset, so a retried transaction does not announce jobs its
		// rolled-back attempt wrote.
		announce = nil
		expired, err := repositories.Orders().ClaimExpired(ctx, s.now(), limit)
		if err != nil {
			return err
		}
		for index := range expired {
			item := expired[index]
			if err := releaseAll(ctx, repositories, &item); err != nil {
				return err
			}
			// The charge goes with the stock, in the same transaction: a code
			// that outlives the seats it was for is money the box office will
			// have to give back.
			if s.voider != nil {
				job, err := s.voider.EnqueueCancelCharge(ctx, repositories.Jobs(), &item)
				if err != nil {
					return err
				}
				if job != nil {
					announce = append(announce, job)
				}
			}
			released++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, job := range announce {
		s.dispatcher.Dispatch(ctx, job)
	}
	return released, nil
}

// ExpireHold releases one order's hold, driven by the job scheduled at its
// expiry. It is a no-op for an order that has since been paid, cancelled, or
// had its window extended by a confirmation, which is the common case on a
// healthy system, because every order schedules one of these at cart time and
// most of them go on to be confirmed.
func (s *Service) ExpireHold(ctx context.Context, orderID string) error {
	var announce *queue.Job
	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		item, err := repositories.Orders().GetByIDForUpdate(ctx, orderID)
		if errors.Is(err, orderdomain.ErrNotFound) {
			// Nothing to expire, and nothing a retry could find.
			return queue.Permanent(fmt.Errorf("expire hold for order %s: %w", orderID, err))
		}
		if err != nil {
			return err
		}
		if !item.HoldLapsed(s.now()) {
			return nil
		}
		changed, err := item.Apply(orderdomain.StatusExpired, s.now())
		if err != nil || !changed {
			return err
		}
		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		if err := releaseAll(ctx, repositories, item); err != nil {
			return err
		}
		if s.voider == nil {
			return nil
		}
		job, err := s.voider.EnqueueCancelCharge(ctx, repositories.Jobs(), item)
		if err != nil {
			return err
		}
		announce = job
		return nil
	})
	if err != nil {
		return err
	}
	s.dispatcher.Dispatch(ctx, announce)
	return nil
}

// releaseAll returns every line's stock, in the order the items are held.
//
// Same order as the reservation took them, for the same reason: two
// transactions touching the same two tiers must agree on which to lock first.
func releaseAll(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) error {
	for _, line := range item.Items {
		if err := repositories.Tickets().Release(ctx, line.TicketID, line.Quantity); err != nil {
			return err
		}
	}
	// And the chairs, if this order named any. One call rather than one per
	// line, because a seat is released by the ORDER holding it; the repository
	// scopes it to held, so this can never take a seat from an order that paid.
	//
	// A counted order matches no rows here and costs one indexed statement
	// against a partial index. Skipping it on a flag would mean trusting a flag
	// to be right about inventory, which is the trade this system does not make.
	return releaseSeats(ctx, repositories, item.ID)
}

func (s *Service) enqueueCharge(ctx context.Context, jobs queue.Queue, item *orderdomain.Order) (*queue.Job, error) {
	payload, err := queue.NewPayload(queue.CreateChargePayload{OrderID: item.ID})
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        queue.TypeCreateCharge,
		Payload:     payload,
		RunAt:       s.now(),
		MaxAttempts: queue.DefaultMaxAttempts,
		// One charge per order, however many times this is enqueued.
		DedupeKey: queue.TypeCreateCharge + ":" + item.ID,
	}
	added, err := jobs.Enqueue(ctx, job)
	if err != nil {
		return nil, err
	}
	if !added {
		// Already scheduled; nothing new to publish.
		return nil, nil
	}
	return job, nil
}

// enqueueExpiry schedules the hold's own expiry. It is never published: the
// broker has no delayed delivery without a plugin, and a job due in ten or
// thirty minutes is exactly what the poller is for.
//
// The dedupe key carries the deadline, so the cart hold and the payment hold
// each get their own job. A key naming only the order would let the second
// enqueue collapse into the first, leaving the extended window with no job of
// its own and the sweep as its only backstop.
func (s *Service) enqueueExpiry(ctx context.Context, jobs queue.Queue, item *orderdomain.Order) (*queue.Job, error) {
	payload, err := queue.NewPayload(queue.SyncPaymentPayload{OrderID: item.ID})
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        queue.TypeExpireHolds,
		Payload:     payload,
		RunAt:       item.HoldExpiresAt,
		MaxAttempts: queue.DefaultMaxAttempts,
		DedupeKey: queue.TypeExpireHolds + ":" + item.ID + ":" +
			strconv.FormatInt(item.HoldExpiresAt.UTC().Unix(), 10),
	}
	if _, err := jobs.Enqueue(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func randomID() string {
	return prefixedID("ord_")
}

func (s *Service) newItemID() string {
	return prefixedID("oi_")
}

func prefixedID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return prefix + time.Now().UTC().Format("20060102150405000000000")
	}
	return prefix + hex.EncodeToString(buffer)
}
