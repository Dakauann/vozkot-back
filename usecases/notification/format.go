package notification

import (
	"strconv"
	"strings"
	"time"

	// The IANA database is compiled into the binary. Without it, a scratch or
	// distroless container silently falls back to UTC and every buyer is told
	// their show starts three hours later than it does.
	_ "time/tzdata"
)

// saoPaulo is the box office's wall clock. Times are stored in UTC and read
// back in UTC everywhere else in this system, which is correct for machines
// and wrong for the one line a buyer actually reads: the door time.
//
// Brazil has observed no daylight saving since 2019, so a fixed UTC-3 is a
// correct fallback on a host without the tz database, which is what keeps a
// misconfigured image from being a silently wrong email.
var saoPaulo = resolveLocation(time.LoadLocation, "America/Sao_Paulo", time.FixedZone("America/Sao_Paulo", -3*60*60))

func resolveLocation(load func(string) (*time.Location, error), name string, fallback *time.Location) *time.Location {
	if location, err := load(name); err == nil {
		return location
	}
	return fallback
}

var months = [...]string{
	"janeiro", "fevereiro", "março", "abril", "maio", "junho",
	"julho", "agosto", "setembro", "outubro", "novembro", "dezembro",
}

var weekdays = [...]string{
	"domingo", "segunda-feira", "terça-feira", "quarta-feira",
	"quinta-feira", "sexta-feira", "sábado",
}

// longDate renders an event's door time the way a Brazilian ticket does:
// "sábado, 13 de setembro de 2026, 22h00".
//
// Written out rather than taken from a localisation library because it is two
// lookup tables and one format string, and the buyer-facing locale of a
// Brazilian box office is not a runtime decision. When emails do become
// multi-locale, this is the function that grows a language argument.
func longDate(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	local := value.In(saoPaulo)
	return weekdays[int(local.Weekday())] + ", " +
		strconv.Itoa(local.Day()) + " de " + months[int(local.Month())-1] +
		" de " + strconv.Itoa(local.Year()) + ", " +
		pad2(local.Hour()) + "h" + pad2(local.Minute())
}

// shortDateTime renders a timestamp for a receipt line: "13/09/2026 22:04".
func shortDateTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	local := value.In(saoPaulo)
	return pad2(local.Day()) + "/" + pad2(int(local.Month())) + "/" + strconv.Itoa(local.Year()) +
		" " + pad2(local.Hour()) + ":" + pad2(local.Minute())
}

// money renders centavos as Brazilian currency: 123456 becomes "R$ 1.234,56".
//
// Integer arithmetic all the way down. Money in this system is int64 centavos
// precisely so it never meets a float, and formatting is the classic place
// that discipline gets thrown away for one convenient division.
func money(cents int64, currency string) string {
	symbol := symbolFor(currency)
	negative := cents < 0
	if negative {
		cents = -cents
	}
	units := cents / 100
	fraction := cents % 100

	digits := strconv.FormatInt(units, 10)
	var grouped strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			grouped.WriteByte('.')
		}
		grouped.WriteRune(digit)
	}

	amount := symbol + " " + grouped.String() + "," + pad2(int(fraction))
	if negative {
		return "-" + amount
	}
	return amount
}

func symbolFor(currency string) string {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "", "BRL":
		return "R$"
	case "USD":
		return "US$"
	case "EUR":
		return "€"
	default:
		return strings.ToUpper(strings.TrimSpace(currency))
	}
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}
