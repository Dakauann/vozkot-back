package notification

import (
	"errors"
	"testing"
	"time"

	paymentdomain "vozkot/domain/payment"
)

// Money is int64 centavos everywhere in this system precisely so it never meets
// a float. Formatting is the classic place that discipline is thrown away for
// one convenient division, so it gets its own test.
func TestMoney(t *testing.T) {
	cases := []struct {
		cents    int64
		currency string
		want     string
	}{
		{24000, "BRL", "R$ 240,00"},
		{0, "BRL", "R$ 0,00"},
		{5, "BRL", "R$ 0,05"},
		{99, "", "R$ 0,99"},
		{100000, "BRL", "R$ 1.000,00"},
		{123456789, "BRL", "R$ 1.234.567,89"},
		{-2500, "BRL", "-R$ 25,00"},
		{100000, "USD", "US$ 1.000,00"},
	}
	for _, testCase := range cases {
		if got := money(testCase.cents, testCase.currency); got != testCase.want {
			t.Errorf("money(%d, %q) = %q, want %q", testCase.cents, testCase.currency, got, testCase.want)
		}
	}
}

// Times are stored in UTC and read back in UTC everywhere else, which is right
// for machines and wrong for the one line a buyer reads. A door time three
// hours out is the difference between arriving and missing the show.
func TestLongDateIsBrazilianWallClock(t *testing.T) {
	// 01:00 UTC on the 4th is 22:00 on the 3rd in São Paulo.
	stored := time.Date(2026, time.October, 4, 1, 0, 0, 0, time.UTC)
	if got, want := longDate(stored), "sábado, 3 de outubro de 2026, 22h00"; got != want {
		t.Fatalf("longDate = %q, want %q", got, want)
	}
	if got := longDate(time.Time{}); got != "" {
		t.Fatalf("longDate(zero) = %q, want empty", got)
	}
}

func TestShortDateTime(t *testing.T) {
	stored := time.Date(2026, time.September, 14, 1, 4, 0, 0, time.UTC)
	if got, want := shortDateTime(stored), "13/09/2026 22:04"; got != want {
		t.Fatalf("shortDateTime = %q, want %q", got, want)
	}
	if got := shortDateTime(time.Time{}); got != "" {
		t.Fatalf("shortDateTime(zero) = %q, want empty", got)
	}
}

// A host without the tz database must not silently move every event three
// hours. The fallback is a fixed UTC-3, which is correct for Brazil since it
// stopped observing daylight saving in 2019.
func TestLocationFallsBackWithoutTZData(t *testing.T) {
	fallback := time.FixedZone("America/Sao_Paulo", -3*60*60)
	got := resolveLocation(func(string) (*time.Location, error) {
		return nil, errors.New("no tz database on this host")
	}, "America/Sao_Paulo", fallback)
	if got != fallback {
		t.Fatalf("resolveLocation returned %v, want the fallback", got)
	}
}

// reference is what a buyer reads out to support: short enough to say, long
// enough to find the row.
func TestReference(t *testing.T) {
	if got, want := reference("ord_a1b2c3d4e5f6"), "A1B2C3D4"; got != want {
		t.Fatalf("reference = %q, want %q", got, want)
	}
	if got, want := reference("ord_ab"), "AB"; got != want {
		t.Fatalf("reference = %q, want %q", got, want)
	}
}

func TestFirstName(t *testing.T) {
	if got, want := firstName("Maria Aparecida dos Santos"), "Maria"; got != want {
		t.Fatalf("firstName = %q, want %q", got, want)
	}
	if got := firstName("   "); got != "" {
		t.Fatalf("firstName(blank) = %q, want empty", got)
	}
}

func TestMethodLabel(t *testing.T) {
	cases := map[paymentdomain.Method]string{
		paymentdomain.MethodPix:    "PIX",
		paymentdomain.MethodBoleto: "Boleto",
		paymentdomain.MethodCard:   "Cartão",
	}
	for method, want := range cases {
		if got := methodLabel(method); got != want {
			t.Errorf("methodLabel(%s) = %q, want %q", method, got, want)
		}
	}
}
