package ledger

import "time"

// Holidays answers whether the banks are shut on a given day.
//
// A function rather than an interface because there is one question, and a port
// rather than a table inside this package because the answer is Brazilian
// national holidays plus, eventually, municipal ones, data that changes every
// year and belongs to infra.
type Holidays func(day time.Time) bool

// NoHolidays is the explicit "weekends only" calendar, for tests and for a
// deployment that has not wired a holiday source yet.
//
// Named rather than passed as nil at call sites, so reading one says which
// calendar was meant instead of leaving a reader to discover that nil has a
// behaviour.
var NoHolidays Holidays = func(time.Time) bool { return false }

// Brasilia is the location a banking day is measured in.
//
// NOT the venue's own timezone, and not UTC. A settlement date is a promise
// about when a BANK moves money, and Brazilian banking days run on Brasília
// time whether the show was in Recife or Rio Branco. Fixed rather than loaded
// from the host, because a server in another region must not shift every
// organiser's payout date by a day.
//
// A fixed -03:00 offset rather than a tzdata lookup: Brazil abolished daylight
// saving in 2019, so the offset is constant, and depending on tzdata would make
// settlement dates depend on whether a scratch container shipped the database.
var Brasilia = time.FixedZone("BRT", -3*60*60)

// Calendar is when the banks are open: which days are holidays, and whose days
// they are.
//
// The two travel together because they answer one question and are wrong apart.
// Counting Brazilian holidays on UTC calendar days is the bug this type exists
// to make unrepresentable: an event ending 23:00 in São Paulo is 02:00 the
// NEXT day in UTC, and a count that started from the UTC day landed a Tuesday
// show's money on the following Monday instead of the Friday.
type Calendar struct {
	Holidays Holidays
	// In is the location days are counted in. Nil means Brasília.
	In *time.Location
}

// BankingCalendar is the default: Brasília days, weekends only, no holiday
// source wired. A deployment with a holiday feed replaces Holidays.
var BankingCalendar = Calendar{Holidays: NoHolidays, In: Brasilia}

func (c Calendar) location() *time.Location {
	if c.In == nil {
		return Brasilia
	}
	return c.In
}

// IsBusinessDay is Monday to Friday in the calendar's own location, minus
// whatever it excludes.
func (c Calendar) IsBusinessDay(day time.Time) bool {
	local := day.In(c.location())
	switch local.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return c.Holidays == nil || !c.Holidays(local)
}

// AddBusinessDays returns the instant `days` banking days after `from`.
//
// The instant is preserved, the same moment, not a midnight, and the day it
// starts from is never itself counted: "three business days after the show"
// means three days on which a bank could have moved money, beginning the day
// after it ended. A show that finishes on a Friday night therefore settles on
// the following Wednesday, which is what every platform researched does and
// what an organiser will check on a calendar.
//
// Zero or fewer days returns `from` unchanged rather than guessing, so a Terms
// with a nonsensical settlement cannot quietly become "immediately".
func (c Calendar) AddBusinessDays(from time.Time, days int) time.Time {
	if days <= 0 {
		return from
	}
	// Walked in the calendar's location so that "the next day" is the next day
	// there, then handed back as the same kind of instant it arrived as.
	day := from.In(c.location())
	for counted := 0; counted < days; {
		day = day.AddDate(0, 0, 1)
		if c.IsBusinessDay(day) {
			counted++
		}
	}
	return day.In(from.Location())
}
