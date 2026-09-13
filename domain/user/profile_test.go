package user

import (
	"errors"
	"testing"
	"time"
)

// Pure domain. The rules here decide whether a real person can buy a ticket and
// whether a document that will be checked at a door is the one on the order, so
// they are tested against real check digits rather than against the shape of
// the string.

var today = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func draft() ProfileDraft {
	return ProfileDraft{
		DocumentType: "cpf",
		// A valid CPF by the Receita Federal's own algorithm.
		Document:  "529.982.247-25",
		LegalName: "Maria  Souza ",
		BirthDate: "1994-03-21",
	}
}

func TestNormalizeProfileStoresTheCanonicalForm(t *testing.T) {
	profile, err := NormalizeProfile(draft(), today)

	if err != nil {
		t.Fatalf("NormalizeProfile() error = %v", err)
	}
	if profile.Document != "52998224725" {
		t.Fatalf("document = %q, want the digits only", profile.Document)
	}
	// Collapsed whitespace, so the same name typed two ways is one value.
	if profile.LegalName != "Maria Souza" {
		t.Fatalf("name = %q, want the collapsed form", profile.LegalName)
	}
	if profile.BirthDate != "1994-03-21" {
		t.Fatalf("birth date = %q", profile.BirthDate)
	}
	if profile.Complete() {
		t.Fatal("a normalized draft reports itself complete; only persisting it should")
	}
}

// TestCPFCheckDigitsAreVerified is the case a length check would let through: a
// transposed digit produces an eleven-digit string that is not a document.
func TestCPFCheckDigitsAreVerified(t *testing.T) {
	valid := []string{"529.982.247-25", "52998224725", "111.444.777-35"}
	for _, document := range valid {
		input := draft()
		input.Document = document
		if _, err := NormalizeProfile(input, today); err != nil {
			t.Fatalf("%s was refused: %v", document, err)
		}
	}

	invalid := map[string]string{
		"transposed digits":  "529.982.247-52",
		"one digit changed":  "52998224726",
		"too short":          "5299822472",
		"too long":           "529982247251",
		"all the same digit": "11111111111",
		"all zeroes":         "00000000000",
		"letters":            "abcdefghijk",
		"empty":              "",
	}
	for name, document := range invalid {
		input := draft()
		input.Document = document
		if _, err := NormalizeProfile(input, today); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("%s (%q): error = %v, want %v", name, document, err, ErrInvalidDocument)
		}
	}
}

func TestCNPJCheckDigitsAreVerified(t *testing.T) {
	input := draft()
	input.DocumentType = "cnpj"
	input.Document = "11.222.333/0001-81"

	profile, err := NormalizeProfile(input, today)
	if err != nil {
		t.Fatalf("a valid CNPJ was refused: %v", err)
	}
	if profile.Document != "11222333000181" {
		t.Fatalf("document = %q, want the digits only", profile.Document)
	}

	for name, document := range map[string]string{
		"one digit changed": "11222333000182",
		"too short":         "1122233300018",
		"all the same":      "11111111111111",
	} {
		bad := input
		bad.Document = document
		if _, err := NormalizeProfile(bad, today); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("%s (%q): error = %v, want %v", name, document, err, ErrInvalidDocument)
		}
	}
}

// TestPassportIsAcceptedWithoutAChecksum: no checksum exists, so the rule is
// shape and canonical case — a buyer at an international event has neither a
// CPF nor a CNPJ and must still be able to buy.
func TestPassportIsAcceptedWithoutAChecksum(t *testing.T) {
	input := draft()
	input.DocumentType = "passport"
	input.Document = " fj 123456 "

	profile, err := NormalizeProfile(input, today)
	if err != nil {
		t.Fatalf("NormalizeProfile() error = %v", err)
	}
	if profile.Document != "FJ123456" {
		t.Fatalf("passport = %q, want it uppercased with spaces removed", profile.Document)
	}

	for name, document := range map[string]string{
		"too short":      "AB12",
		"too long":       "ABCDEFGHIJKLMNOPQRSTUV",
		"has separators": "FJ-123456",
	} {
		bad := input
		bad.Document = document
		if _, err := NormalizeProfile(bad, today); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("%s (%q): error = %v, want %v", name, document, err, ErrInvalidDocument)
		}
	}
}

func TestAFullNameIsRequired(t *testing.T) {
	for name, value := range map[string]string{
		"empty":      "   ",
		"first only": "Maria",
		"too short":  "M S",
	} {
		input := draft()
		input.LegalName = value
		_, err := NormalizeProfile(input, today)
		if value == "M S" {
			// Three runes with a space is the shortest plausible real name and
			// is allowed; the assertion is only that it is not rejected.
			if err != nil {
				t.Fatalf("%s (%q) was refused: %v", name, value, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidLegalName) {
			t.Fatalf("%s (%q): error = %v, want %v", name, value, err, ErrInvalidLegalName)
		}
	}
}

func TestBirthDateIsBoundedAtBothEnds(t *testing.T) {
	for name, value := range map[string]struct {
		date string
		want error
	}{
		"not a date":    {"21/03/1994", ErrInvalidBirthDate},
		"in the future": {"2027-01-01", ErrInvalidBirthDate},
		"implausible":   {"1890-01-01", ErrInvalidBirthDate},
		"empty":         {"", ErrInvalidBirthDate},
	} {
		input := draft()
		input.BirthDate = value.date
		if _, err := NormalizeProfile(input, today); !errors.Is(err, value.want) {
			t.Fatalf("%s (%q): error = %v, want %v", name, value.date, err, value.want)
		}
	}
}

// TestAgeIsCountedByBirthdayNotByDivision.
//
// Somebody who turns sixteen tomorrow is fifteen today. Dividing elapsed days
// by 365.25 gets this wrong for a few days a year, and those are exactly the
// days the check exists for.
func TestAgeIsCountedByBirthdayNotByDivision(t *testing.T) {
	input := draft()

	// Sixteen tomorrow.
	input.BirthDate = "2010-09-14"
	if _, err := NormalizeProfile(input, today); !errors.Is(err, ErrUnderage) {
		t.Fatalf("someone who turns 16 tomorrow was allowed: %v", err)
	}

	// Sixteen today.
	input.BirthDate = "2010-09-13"
	if _, err := NormalizeProfile(input, today); err != nil {
		t.Fatalf("someone who turns 16 today was refused: %v", err)
	}
}

func TestDocumentTypeIsChecked(t *testing.T) {
	for _, value := range []string{"", "rg", "driver_licence", "CPF "} {
		input := draft()
		input.DocumentType = value
		_, err := NormalizeProfile(input, today)
		if value == "CPF " {
			// Case and surrounding space are normalised, not rejected.
			if err != nil {
				t.Fatalf("%q was refused: %v", value, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidDocumentType) {
			t.Fatalf("%q: error = %v, want %v", value, err, ErrInvalidDocumentType)
		}
	}
}

func TestNormalizePhoneReducesSpellingsToOneValue(t *testing.T) {
	for _, spelling := range []string{"(84) 99440-9624", "84994409624", "+55 84 99440-9624", "5584994409624"} {
		got, err := NormalizePhone(spelling)
		if err != nil {
			t.Fatalf("%q was refused: %v", spelling, err)
		}
		if got != "5584994409624" {
			t.Fatalf("%q normalised to %q, want 5584994409624", spelling, got)
		}
	}

	for _, bad := range []string{"", "123", "abc", "1234567890123456789"} {
		if _, err := NormalizePhone(bad); !errors.Is(err, ErrInvalidPhone) {
			t.Fatalf("%q: error = %v, want %v", bad, err, ErrInvalidPhone)
		}
	}
}
