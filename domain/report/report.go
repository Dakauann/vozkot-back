// Package report is what an organiser is allowed to know about the people who
// bought their tickets.
//
// Two shapes, and the difference between them is the whole privacy posture of
// this feature:
//
//   - AGGREGATES: how many tickets went to which state, age band, gender and
//     day. Nobody is named. This is what the organiser's dashboard renders and
//     it is computed by GROUP BY over the coarse snapshot frozen onto each
//     order, never by decrypting anybody's profile.
//   - ATTENDEES: the list, one row per order line, with a name and an email on
//     it, because an organiser genuinely has to be able to contact the people
//     coming to their event and to check somebody in at the door.
//
// What is deliberately NOT here: the buyer's document in full, their date of
// birth, and their phone. A CPF is what makes identity theft possible and an
// organiser has no operational use for one, so the export carries a masked
// document, enough to match a person to their ID at the door, not enough to
// be them anywhere else. See docs/REFUNDS_AND_PAYOUTS.md and the note on
// sealing in domain/user.
//
// MONEY IN THIS PACKAGE IS THE ORGANISER'S EARNINGS AND NOTHING ELSE.
//
// Every amount here is NET: the face value the organiser priced, which is what
// they are owed. The service fee this box office adds on top, and therefore the
// gross the buyer actually paid, are deliberately absent from these types.
//
// That is a privacy decision about OUR commercial terms, and it is enforced by
// absence rather than by a permission check. The alternative, carrying gross
// and fee through the repository, the use case and the DTO and stripping them
// for non-administrators at the edge, puts the whole thing one forgotten
// conditional away from showing an organiser our margin, and a response shape
// that varies by role is exactly the kind of surface where that conditional
// gets forgotten. There is nothing to strip here because there is nothing to
// leak: the queries do not select it.
//
// The buyer's side is the opposite and must stay that way: a buyer sees the fee
// itemised before they pay, because Decreto 7.962/2013 art. 2, IV-V requires it
// and STJ REsp 1.632.928 makes that disclosure the compliance obligation. See
// delivery/http/checkout and delivery/http/event.
//
// Our own revenue is not reported here. It lives on orders.service_fee_cents,
// which is the source of truth for it, and reaches the logs on every charge.
package report

import (
	"context"
	"time"
)

// UnknownKey is the bucket every breakdown puts unanswered questions in.
//
// A real row rather than a dropped one. An audience report whose percentages
// are computed over only the people who filled in a field is a report that
// silently overstates every share it shows; "Não informado: 9.877" is the row
// that keeps the rest honest.
const UnknownKey = "unknown"

// AgeBracket is the band a report groups buyers into.
//
// The bands are five years wide from 19 and open at both ends, which is the
// grouping Brazilian ticketing dashboards use and the one an organiser already
// knows how to read. They are strings rather than computed ranges so that the
// API, the CSV and the chart all name a band the same way.
type AgeBracket string

const (
	AgeUpTo18  AgeBracket = "up_to_18"
	Age19To23  AgeBracket = "19_23"
	Age24To28  AgeBracket = "24_28"
	Age29To33  AgeBracket = "29_33"
	Age34To38  AgeBracket = "34_38"
	Age39To43  AgeBracket = "39_43"
	Age44To48  AgeBracket = "44_48"
	Age49To53  AgeBracket = "49_53"
	Age54To58  AgeBracket = "54_58"
	Age59Plus  AgeBracket = "59_plus"
	AgeUnknown AgeBracket = UnknownKey
)

// AgeBrackets is every band, youngest first, with the unknown bucket last,
// the order a table should render them in.
func AgeBrackets() []AgeBracket {
	return []AgeBracket{
		AgeUpTo18, Age19To23, Age24To28, Age29To33, Age34To38,
		Age39To43, Age44To48, Age49To53, Age54To58, Age59Plus, AgeUnknown,
	}
}

// BracketFor maps an age in whole years onto its band.
//
// Zero means "no date of birth", which is why it is the unknown bucket rather
// than "up to 18": a newborn does not buy tickets, and treating a missing value
// as the youngest band would invent an audience the organiser does not have.
// The profile floors a real age at user.MinimumAge, so the up-to-18 band holds
// buyers at exactly that floor rather than a spread of children.
func BracketFor(ageYears int) AgeBracket {
	switch {
	case ageYears <= 0:
		return AgeUnknown
	case ageYears <= 18:
		return AgeUpTo18
	case ageYears <= 23:
		return Age19To23
	case ageYears <= 28:
		return Age24To28
	case ageYears <= 33:
		return Age29To33
	case ageYears <= 38:
		return Age34To38
	case ageYears <= 43:
		return Age39To43
	case ageYears <= 48:
		return Age44To48
	case ageYears <= 53:
		return Age49To53
	case ageYears <= 58:
		return Age54To58
	default:
		return Age59Plus
	}
}

// Slice is one row of a breakdown.
//
// Orders and Tickets are both carried because they answer different questions
// and an organiser asks both: "how many people" and "how many seats". One order
// for eight tickets is one buyer and eight admissions, and a table that showed
// only one of those would be wrong for half the decisions made from it.
type Slice struct {
	// Key is the bucket: a UF, a gender, an age bracket, a tier id, a date.
	// UnknownKey when the buyer did not say.
	Key string
	// Label is a human-readable name where the key is an id: a tier's title.
	// Empty where the key is already the label, and the client translates it.
	Label   string
	Orders  int
	Tickets int
	// NetCents is the organiser's earnings for this slice: the face value they
	// priced, with none of our commission in it. The only money in this type.
	NetCents int64
}

// Totals is the headline, and the denominator every share on the page is
// computed against.
type Totals struct {
	Orders  int
	Tickets int
	// Buyers is DISTINCT accounts, which is the number an organiser means by
	// "how many people came". It differs from Orders the moment anybody buys
	// twice, and it is the honest denominator for the demographic breakdowns:
	// a buyer who ordered three times is one person in one age band, not three.
	Buyers int
	// NetCents is the organiser's earnings: the sum of the face values.
	NetCents int64
	// RefundedOrders and RefundedCents are what went back. Reported beside the
	// sales rather than deducted from them, because "we sold 500 and refunded
	// 12" and "we sold 488" are different facts and only the first can be
	// acted on.
	//
	// RefundedCents is NET too, for a reason that is easy to miss: a refund
	// returns the service fee to the buyer as well, so a refunded figure at
	// gross next to a sales figure at net would let the fee be recovered by
	// subtracting one from the other. It is also the more useful number:
	// what the organiser gave back is their share, not ours.
	RefundedOrders int
	RefundedCents  int64
}

// AverageOrderCents is the ticket médio every platform of this kind reports:
// what a buyer spends in one go.
//
// DERIVED, not stored and not a seventh query. It is a ratio of two numbers
// already here, and computing it in SQL would be a second definition of "an
// order" that could disagree with Totals.Orders the first time either changed.
//
// Integer division, rounded half-up, so it is a centavo figure like every other
// amount in this package rather than a float that renders differently in three
// places. Zero orders is zero rather than a division by zero: an event that has
// sold nothing has no average, and reporting one would be inventing a number.
func (t Totals) AverageOrderCents() int64 {
	if t.Orders <= 0 {
		return 0
	}
	return (t.NetCents*2 + int64(t.Orders)) / (int64(t.Orders) * 2)
}

// AverageTicketCents is the same question asked per ADMISSION rather than per
// order, which is the one an organiser uses to price a tier. A party where one
// buyer takes eight tickets has a high average order and an ordinary average
// ticket, and confusing the two is how a tier gets repriced on the wrong
// evidence.
func (t Totals) AverageTicketCents() int64 {
	if t.Tickets <= 0 {
		return 0
	}
	return (t.NetCents*2 + int64(t.Tickets)) / (int64(t.Tickets) * 2)
}

// Sales is the whole dashboard for one event.
type Sales struct {
	EventID  string
	Totals   Totals
	ByGender []Slice
	ByAge    []Slice
	ByUF     []Slice
	ByCity   []Slice
	ByTier   []Slice
	// ByDay is ordered oldest first, which is what a sales curve is read in.
	ByDay []DaySlice
}

// DaySlice is one calendar day of sales.
type DaySlice struct {
	// Day is midnight UTC on the day the order was PAID. Paid and not created:
	// a basket opened on Monday and paid on Thursday is Thursday's revenue,
	// and a curve drawn on creation counts holds that never became money.
	Day     time.Time
	Orders  int
	Tickets int
	// NetCents is the organiser's earnings for the day.
	NetCents int64
}

// Attendee is one line of the list an organiser exports.
//
// One row per ORDER LINE rather than per order, because "3× Pista" and
// "1× Camarote" on one order are two different admissions with two different
// prices, and a box office reconciling its door against its sales needs them
// apart. Quantity stays on the row: this product sells a quantity of a tier,
// not a named seat, so exploding it into three identical rows would invent a
// precision the data does not have.
type Attendee struct {
	OrderID     string
	PurchasedAt time.Time
	// Status is the order's status, carried so an export can be filtered and so
	// a refunded row is visibly refunded rather than silently missing.
	Status string

	Name  string
	Email string
	// DocumentMask is the first three and last two digits, the convention every
	// Brazilian service uses. Never the whole number: see the package comment.
	DocumentMask string

	Gender   string
	AgeYears int
	City     string
	UF       string

	TicketID    string
	TicketTitle string
	Quantity    int
	// UnitPriceCents is the face value of one ticket on this line and NetCents
	// is that times the quantity: the organiser's earnings from the line.
	//
	// What the buyer paid is not here. It is the face value plus our
	// commission, so carrying it would put the commission one subtraction away
	// on every row of an export the organiser opens in a spreadsheet.
	UnitPriceCents int64
	NetCents       int64
	// AdmittedCount is how many of this line's tickets have come through the
	// door, out of Quantity.
	//
	// A COUNT and not a boolean, because a line is a quantity: an order for
	// three Pista where two people have arrived is neither "checked in" nor
	// "not checked in", and a boolean would have to lie in one direction. The
	// organiser standing at the door wants the number.
	AdmittedCount int
}

// AttendeeFilter selects rows for the list and the export.
type AttendeeFilter struct {
	EventID string
	// TicketID narrows to one tier.
	TicketID string
	// Status defaults to paid at the use-case level: "who is coming" is a
	// question about people who actually paid, and an export that silently
	// included expired holds would have an organiser emailing people who never
	// bought anything.
	Status string
	// Query matches a name or an email, for finding one person at the door.
	Query  string
	Limit  int
	Offset int
}

// MaxPageSize bounds a listing request. The export is not paginated and is
// bounded by MaxExportRows instead.
const MaxPageSize = 200

// DefaultPageSize fills a table without a scroll to nowhere.
const DefaultPageSize = 50

// MaxExportRows caps one CSV.
//
// An export is a full table scan streamed to a browser, and an arena is fifty
// thousand orders: unbounded, it is both the slowest query this system can be
// asked for and a way to hold a database connection open for minutes. The cap
// is high enough for any real event and low enough to stay a request rather
// than a job; past it, the honest answer is to filter by tier.
const MaxExportRows = 100_000

// Normalize clamps a filter into the range the repository will honour.
func (f AttendeeFilter) Normalize() AttendeeFilter {
	if f.Limit <= 0 {
		f.Limit = DefaultPageSize
	}
	if f.Limit > MaxPageSize {
		f.Limit = MaxPageSize
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

// Page is a listing plus the total it was taken from.
type Page struct {
	Items []Attendee
	Total int64
}

// Repository reads the numbers. It is READ-ONLY by design: nothing in this
// package may write, so no reporting change can ever move money or inventory.
type Repository interface {
	// Sales computes every breakdown for one scope: an event, or an
	// organiser's whole portfolio. See Scope.
	Sales(ctx context.Context, scope Scope) (Sales, error)
	// Attendees lists the people who bought.
	Attendees(ctx context.Context, filter AttendeeFilter) (Page, error)
	// StreamAttendees hands rows to fn in batches, for the export.
	//
	// A callback rather than a slice because the export must not hold an
	// arena's worth of rows in memory to write them one at a time to a socket;
	// fn is called with each batch and the rows are written and dropped.
	StreamAttendees(ctx context.Context, filter AttendeeFilter, fn func([]Attendee) error) error
}

// Scope is WHOSE numbers a report covers: one event, or everything one
// organiser has ever sold.
//
// A value rather than two methods on the repository, because the six breakdowns
// below differ only in that clause. Two sets of near-identical SQL is how a
// portfolio total and an event total start disagreeing about what a refund is,
// and the disagreement surfaces months later as a number an organiser cannot
// reconcile with the sum of their own events.
//
// Exactly one field is set. Both, or neither, is a programming error rather
// than a query: an empty scope would read every order on the platform.
type Scope struct {
	// EventID narrows to a single show.
	EventID string
	// OrganiserID covers every event that organiser owns.
	OrganiserID string
}

// EventScope and OrganiserScope are the two constructors. Named so a call site
// reads as the question it is asking.
func EventScope(eventID string) Scope         { return Scope{EventID: eventID} }
func OrganiserScope(organiserID string) Scope { return Scope{OrganiserID: organiserID} }

// Valid reports whether this scope names exactly one subject.
//
// The zero Scope is INVALID on purpose. A report that silently covered the
// whole platform because an id was empty is the one bug in this package that
// would leak every organiser's revenue to whoever asked first, so the failure
// mode is no rows rather than all of them.
func (s Scope) Valid() bool {
	return (s.EventID != "") != (s.OrganiserID != "")
}
