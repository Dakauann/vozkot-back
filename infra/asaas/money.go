package asaas

import "math"

// The one place reais meet centavos.
//
// This system stores and reasons about money as int64 centavos, everywhere,
// deliberately: integers add up. Asaas's API speaks reais as JSON numbers, which
// decode into float64, and float64 cannot represent 0.10 exactly, so arithmetic
// on them drifts. R$ 0,07 + R$ 0,01 is famously not R$ 0,08 in binary floating
// point.
//
// The rule this file exists to enforce: a float touches money ONLY while it is
// in transit to or from Asaas, is never stored, and is never added to another
// float. Every amount crosses back to an integer the moment it arrives.

// toReais converts centavos to the number Asaas expects.
//
// Exact for every value a charge can hold: dividing an integer by 100 is
// representable to well beyond the largest plausible ticket price, and the
// result is used immediately as a request field rather than kept.
func toReais(cents int64) float64 {
	return float64(cents) / 100
}

// toCents converts an amount Asaas returned back to the only representation
// this system stores.
//
// math.Round, not a truncation: 10.99 decodes as 10.989999999999999787, and
// int64(x*100) on that is 1098: one centavo lost on a charge, silently, every
// time. Rounding to the nearest centavo is correct because the value really is
// a centavo-denominated amount that took a lossy trip through a float.
func toCents(reais float64) int64 {
	return int64(math.Round(reais * 100))
}
