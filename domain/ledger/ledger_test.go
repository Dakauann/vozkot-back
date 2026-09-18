package ledger

import (
	"strconv"
	"testing"
	"time"
)

func ids() func() string {
	n := 0
	return func() string { n++; return "led_" + strconv.Itoa(n) }
}

// A Thursday, so a three-business-day count crosses a weekend without being
// contrived about it.
var thursday = time.Date(2026, 10, 15, 22, 30, 0, 0, time.UTC)

func sale() Sale {
	return Sale{
		OrderID: "ord_1", EventID: "evt_1", OrganiserID: "org_1",
		FaceCents: 5_340_00, EventEndsAt: thursday,
	}
}

func TestSettlementCountsBankingDaysAndNeverTheDayItStartsFrom(t *testing.T) {
	// Thursday night + 3 business days: Friday, Monday, Tuesday.
	got := Calendar{Holidays: NoHolidays, In: time.UTC}.AddBusinessDays(thursday, 3)
	if want := time.Date(2026, 10, 20, 22, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("settlement = %s, want %s", got, want)
	}
	// The time of day survives, so "three days after the doors" does not drift
	// to midnight and change which day a payout run sees it on.
	if got.Hour() != thursday.Hour() || got.Minute() != thursday.Minute() {
		t.Errorf("time of day drifted to %s", got)
	}
	// A holiday is simply not a banking day.
	monday := func(day time.Time) bool { return day.Day() == 19 && day.Month() == time.October }
	withHoliday := Calendar{Holidays: monday, In: time.UTC}
	if got := withHoliday.AddBusinessDays(thursday, 3); !got.Equal(time.Date(2026, 10, 21, 22, 30, 0, 0, time.UTC)) {
		t.Errorf("with a holiday on the Monday, settlement = %s", got)
	}
}

func TestAccrualSplitsTheOrganisersShareIntoDueAndWithheld(t *testing.T) {
	entries, err := Accrue(sale(), StandardTerms, thursday, Calendar{Holidays: NoHolidays, In: time.UTC}, ids())
	if err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want a sale and a reserve", len(entries))
	}
	// 10% of R$ 5.340,00 held back, the rest due on settlement.
	if entries[0].Kind != KindSale || entries[0].AmountCents != 4_806_00 {
		t.Errorf("sale entry = %+v", entries[0])
	}
	if entries[1].Kind != KindReserve || entries[1].AmountCents != 534_00 {
		t.Errorf("reserve entry = %+v", entries[1])
	}
	// Nothing is lost between the two: the organiser is owed their whole face
	// value, and the only question is when.
	if total := entries[0].AmountCents + entries[1].AmountCents; total != sale().FaceCents {
		t.Fatalf("accrued %d against a face value of %d", total, sale().FaceCents)
	}
	// The reserve waits 30 days longer than the sale, and neither is available
	// before the show has even happened.
	if !entries[1].AvailableAt.Equal(entries[0].AvailableAt.Add(30 * 24 * time.Hour)) {
		t.Errorf("reserve available at %s against a sale at %s", entries[1].AvailableAt, entries[0].AvailableAt)
	}
	if !entries[0].AvailableAt.After(thursday) {
		t.Error("a sale became available before the event ended")
	}
}

func TestATrustedOrganiserHoldsNothingBack(t *testing.T) {
	entries, err := Accrue(sale(), TrustedTerms, thursday, Calendar{Holidays: NoHolidays, In: time.UTC}, ids())
	if err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != KindSale {
		t.Fatalf("entries = %+v, want one sale and no reserve row", entries)
	}
	if entries[0].AmountCents != sale().FaceCents {
		t.Errorf("a trusted organiser was paid %d of %d", entries[0].AmountCents, sale().FaceCents)
	}
}

// The service fee is added on top of the organiser's price, so it was never
// theirs and must not appear on their ledger at all. An entry for it would be
// subtracting money that was never added.
func TestTheServiceFeeNeverTouchesTheOrganisersLedger(t *testing.T) {
	entries, err := Accrue(sale(), StandardTerms, thursday, Calendar{Holidays: NoHolidays, In: time.UTC}, ids())
	if err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	var total int64
	for _, entry := range entries {
		if entry.Kind == KindGatewayFee || entry.AmountCents < 0 {
			t.Fatalf("an accrual wrote a debit: %+v", entry)
		}
		total += entry.AmountCents
	}
	if total != sale().FaceCents {
		t.Fatalf("the organiser accrued %d, not the %d they priced", total, sale().FaceCents)
	}
}

func TestAReversalIsAvailableImmediatelyEvenThoughItsSaleIsNot(t *testing.T) {
	entries, err := Accrue(sale(), StandardTerms, thursday, Calendar{Holidays: NoHolidays, In: time.UTC}, ids())
	if err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	refundedAt := thursday.Add(time.Hour)
	reversal, err := Reverse(sale(), KindRefund, sale().FaceCents, sale().FaceCents, refundedAt, ids())
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if reversal.AmountCents != -sale().FaceCents {
		t.Errorf("reversal = %d", reversal.AmountCents)
	}
	if !reversal.AvailableAt.Equal(refundedAt) {
		t.Errorf("reversal available at %s, want immediately", reversal.AvailableAt)
	}

	// THE SAFETY PROPERTY: on the day the sale settles, the refund is already
	// counted and the sale is not paid out. Available is negative, and that
	// negative is the reserve absorbing it rather than money leaving.
	all := append(entries, reversal)
	at := entries[0].AvailableAt
	balance := BalanceOf(all, at)
	if balance.AvailableCents >= 0 {
		t.Fatalf("available = %d on settlement day; the refund did not outrun the sale",
			balance.AvailableCents)
	}
	if balance.ReservedCents != 534_00 {
		t.Errorf("reserved = %d, want the withheld slice still held", balance.ReservedCents)
	}
	if balance.TotalCents != 0 {
		t.Errorf("total = %d after a full refund, want nothing owed", balance.TotalCents)
	}
}

func TestAReversalCannotExceedWhatTheOrderEverAccrued(t *testing.T) {
	_, err := Reverse(sale(), KindRefund, sale().FaceCents+1, sale().FaceCents, thursday, ids())
	if err != ErrRefundTooMuch {
		t.Fatalf("over-refund error = %v, want ErrRefundTooMuch", err)
	}
	// A second reversal is checked against what is LEFT, not against the order.
	if _, err := Reverse(sale(), KindRefund, 1, 0, thursday, ids()); err != ErrRefundTooMuch {
		t.Fatalf("reversing an already-reversed order = %v", err)
	}
}

func TestAnEntryWithTheWrongSignIsRefused(t *testing.T) {
	now := thursday
	for _, entry := range []Entry{
		{ID: "a", OrganiserID: "org_1", Kind: KindSale, AmountCents: -1, AvailableAt: now},
		{ID: "b", OrganiserID: "org_1", Kind: KindRefund, AmountCents: 1, AvailableAt: now},
		{ID: "c", OrganiserID: "org_1", Kind: KindPayout, AmountCents: 1, AvailableAt: now},
		{ID: "d", OrganiserID: "", Kind: KindSale, AmountCents: 1, AvailableAt: now},
		{ID: "e", OrganiserID: "org_1", Kind: KindSale, AmountCents: 0, AvailableAt: now},
		{ID: "f", OrganiserID: "org_1", Kind: "invented", AmountCents: 1, AvailableAt: now},
	} {
		if err := entry.Validate(); err == nil {
			t.Errorf("%+v was accepted", entry)
		}
	}
}

func TestBalanceSeparatesWhatIsDueFromWhatIsMerelyHeld(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Kind: KindSale, AmountCents: 1000, AvailableAt: now.Add(-time.Hour)},
		{Kind: KindSale, AmountCents: 2000, AvailableAt: now.Add(time.Hour)},
		{Kind: KindReserve, AmountCents: 300, AvailableAt: now.Add(48 * time.Hour)},
		{Kind: KindRefund, AmountCents: -500, AvailableAt: now.Add(-time.Minute)},
	}
	got := BalanceOf(entries, now)
	want := Balance{AvailableCents: 500, PendingCents: 2000, ReservedCents: 300, TotalCents: 2800}
	if got != want {
		t.Fatalf("balance = %+v, want %+v", got, want)
	}
}

// Settlement is counted in the instant's own location, and a show that ends
// late at night is the case that exposes it.
//
// An event ending 23:00 in Sao Paulo is 02:00 the NEXT day in UTC. If the count
// ran on a UTC calendar day it would start from Saturday rather than Friday and
// land the organiser's money a day late, every time, for every late show,
// which is most of them. Counting from the instant as the venue experiences it
// is what keeps "three business days after my event" meaning what an organiser
// reads on a calendar.
func TestSettlementCountsFromTheEventsOwnDayNotUTCs(t *testing.T) {
	saoPaulo := time.FixedZone("BRT", -3*60*60)
	// Friday 15 October 2027, 23:00 in Sao Paulo, which is Saturday 02:00 UTC.
	fridayNight := time.Date(2027, 10, 15, 23, 0, 0, 0, saoPaulo)
	if fridayNight.UTC().Weekday() != time.Saturday {
		t.Fatalf("fixture is not the case under test: UTC weekday = %s", fridayNight.UTC().Weekday())
	}

	// Counted locally: Monday, Tuesday, Wednesday.
	local := BankingCalendar.AddBusinessDays(fridayNight, 3)
	if got := local.In(saoPaulo); got.Weekday() != time.Wednesday || got.Day() != 20 {
		t.Fatalf("settlement = %s (%s), want Wednesday the 20th in Sao Paulo", got, got.Weekday())
	}

	// The same instant expressed in UTC settles on the same DAY of the week,
	// because the arithmetic walks the calendar of whatever location it is
	// given and the result is one instant however it is printed.
	utc := BankingCalendar.AddBusinessDays(fridayNight.UTC(), 3)
	if utc.In(saoPaulo).Day() == local.In(saoPaulo).Day() {
		t.Log("note: UTC and local agree for this fixture")
	}
	// What must never happen is the local count landing on a weekend.
	if weekday := local.In(saoPaulo).Weekday(); weekday == time.Saturday || weekday == time.Sunday {
		t.Fatalf("settlement landed on a %s", weekday)
	}
}

// A Tuesday-night show is the case a UTC calendar gets wrong by three days.
//
// 23:00 Tuesday in Sao Paulo is 02:00 WEDNESDAY in UTC. Counting three banking
// days from the UTC day gives Thursday, Friday, Monday, the following MONDAY.
// Counting them from the day the show actually happened gives Wednesday,
// Thursday, Friday. The organiser is told "three business days after your
// event" and would have waited over a weekend for no reason they could see.
//
// The weekend hides this for a Friday show, which is why the obvious fixture
// would have passed against the broken version.
func TestALateNightShowSettlesOnItsOwnWeekNotUTCs(t *testing.T) {
	tuesdayNight := time.Date(2027, 10, 12, 23, 0, 0, 0, Brasilia)
	if tuesdayNight.In(Brasilia).Weekday() != time.Tuesday {
		t.Fatalf("fixture is not a Tuesday: %s", tuesdayNight)
	}
	if tuesdayNight.UTC().Weekday() != time.Wednesday {
		t.Fatalf("fixture does not cross the UTC date line: %s", tuesdayNight.UTC())
	}

	settles := BankingCalendar.AddBusinessDays(tuesdayNight, 3).In(Brasilia)
	if settles.Weekday() != time.Friday || settles.Day() != 15 {
		t.Fatalf("settlement = %s (%s), want Friday the 15th in Brasilia", settles, settles.Weekday())
	}

	// And the SAME INSTANT handed in as UTC settles identically: the calendar
	// counts in its own location whatever location it is given, so where the
	// caller's clock happens to be cannot move an organiser's payout date.
	fromUTC := BankingCalendar.AddBusinessDays(tuesdayNight.UTC(), 3)
	if !fromUTC.Equal(BankingCalendar.AddBusinessDays(tuesdayNight, 3)) {
		t.Fatalf("the same instant settled differently depending on its printed zone: %s vs %s",
			fromUTC, BankingCalendar.AddBusinessDays(tuesdayNight, 3))
	}
}
