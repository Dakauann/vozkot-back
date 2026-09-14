package user

import (
	"errors"
	"strings"
	"time"
)

// The identity a ticket sale legally has to carry, and nothing beyond it.
//
// Brazilian event ticketing needs a document behind a purchase: meia-entrada is
// capped per CPF by law, purchase limits are enforced per document, and a PIX
// charge cannot be issued without one. That is the reason these fields exist,
// not a profile, not a marketing record. Everything here is asked for because
// something refuses to work without it.
//
// Which is also why every one of them is encrypted at rest. A document number,
// a legal name and a date of birth together are enough to impersonate somebody
// to a bank; holding them in plaintext because they were convenient to query is
// the decision that turns a database leak into identity theft.

// DocumentType is which of Brazil's identifiers a buyer gave.
type DocumentType string

const (
	// DocumentCPF is the individual taxpayer number: eleven digits.
	DocumentCPF DocumentType = "cpf"
	// DocumentCNPJ is the company number, for an organiser buying as a
	// business: fourteen digits.
	DocumentCNPJ DocumentType = "cnpj"
	// DocumentPassport is for buyers who have neither, which at an
	// international event is a real and ordinary case.
	DocumentPassport DocumentType = "passport"
)

func (d DocumentType) Valid() bool {
	switch d {
	case DocumentCPF, DocumentCNPJ, DocumentPassport:
		return true
	default:
		return false
	}
}

var (
	ErrInvalidDocumentType = errors.New("document type is invalid")
	ErrInvalidDocument     = errors.New("document number is invalid")
	ErrInvalidLegalName    = errors.New("full name is required")
	ErrInvalidBirthDate    = errors.New("date of birth is invalid")
	ErrUnderage            = errors.New("you must be at least 16 to buy tickets")
	ErrInvalidPhone        = errors.New("phone number is invalid")
	// ErrDocumentInUse is one document already attached to another account.
	// It is what stops the per-document purchase caps being sidestepped by
	// making a second account with the same CPF.
	ErrDocumentInUse = errors.New("this document is already linked to another account")
)

// MinimumAge is the floor for holding an account that can buy.
//
// Sixteen, which is where Brazilian consumer practice sits for a purchase made
// in one's own name. It is a real check rather than a checkbox: a date of birth
// that is being collected anyway should be used for the one thing it is for.
const MinimumAge = 16

// Profile is the identity block on a user.
//
// Every field is optional in the struct and mandatory in Complete: an account
// exists from the moment an email is proven, and the identity is asked for
// afterwards. Forcing it up front would put a document form between a person
// and the ticket they came to buy.
type Profile struct {
	DocumentType DocumentType
	// Document is digits only for CPF and CNPJ; a passport keeps its letters.
	Document  string
	LegalName string
	// BirthDate is a date, not an instant. It is stored as YYYY-MM-DD text
	// because a timestamp would drag a timezone into a value that has none,
	// and "born on the 1st" must not become the 31st in another offset.
	BirthDate string
	Phone     string

	PhoneVerifiedAt *time.Time
	CompletedAt     *time.Time
}

// Complete reports whether the legally required block is filled in.
func (p Profile) Complete() bool { return p.CompletedAt != nil }

// PhoneVerified reports whether the number was proven, not merely typed.
func (p Profile) PhoneVerified() bool { return p.PhoneVerifiedAt != nil }

// ProfileDraft is what a buyer supplies.
type ProfileDraft struct {
	DocumentType string
	Document     string
	LegalName    string
	BirthDate    string
}

// NormalizeProfile validates a draft and returns the storable form.
//
// Validation happens HERE, in the domain, rather than at the HTTP edge, because
// the same rules have to hold for an operator creating a buyer at the door and
// for any future importer. An edge that validates is an edge; a domain that
// validates is the rule.
func NormalizeProfile(draft ProfileDraft, now time.Time) (Profile, error) {
	documentType := DocumentType(strings.ToLower(strings.TrimSpace(draft.DocumentType)))
	if !documentType.Valid() {
		return Profile{}, ErrInvalidDocumentType
	}

	document, err := normalizeDocument(documentType, draft.Document)
	if err != nil {
		return Profile{}, err
	}

	legalName := strings.Join(strings.Fields(draft.LegalName), " ")
	// A full name, not a first name: it is the name that has to match the
	// document at the door.
	if len([]rune(legalName)) < 3 || !strings.Contains(legalName, " ") {
		return Profile{}, ErrInvalidLegalName
	}

	birth, err := time.Parse("2006-01-02", strings.TrimSpace(draft.BirthDate))
	if err != nil {
		return Profile{}, ErrInvalidBirthDate
	}
	today := now.UTC()
	if birth.After(today) {
		return Profile{}, ErrInvalidBirthDate
	}
	// A plausibility floor as well as a ceiling: 1890 is a typo, not a buyer.
	if birth.Year() < 1900 {
		return Profile{}, ErrInvalidBirthDate
	}
	if ageOn(birth, today) < MinimumAge {
		return Profile{}, ErrUnderage
	}

	return Profile{
		DocumentType: documentType,
		Document:     document,
		LegalName:    legalName,
		BirthDate:    birth.Format("2006-01-02"),
	}, nil
}

// normalizeDocument strips formatting and checks the shape of each type.
//
// CPF gets its check digits verified, which is worth doing: it costs nothing
// and it catches a transposed digit at the point the buyer can still fix it,
// rather than at the door.
func normalizeDocument(documentType DocumentType, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidDocument
	}

	switch documentType {
	case DocumentCPF:
		digits := onlyDigits(trimmed)
		if len(digits) != 11 || !validCPF(digits) {
			return "", ErrInvalidDocument
		}
		return digits, nil
	case DocumentCNPJ:
		digits := onlyDigits(trimmed)
		if len(digits) != 14 || !validCNPJ(digits) {
			return "", ErrInvalidDocument
		}
		return digits, nil
	case DocumentPassport:
		// No checksum exists to verify. Bounded and uppercased, so the same
		// passport typed two ways indexes identically.
		passport := strings.ToUpper(strings.ReplaceAll(trimmed, " ", ""))
		if len(passport) < 5 || len(passport) > 20 || !alphanumeric(passport) {
			return "", ErrInvalidDocument
		}
		return passport, nil
	default:
		return "", ErrInvalidDocumentType
	}
}

// NormalizePhone reduces a Brazilian mobile number to digits with its country
// code, which is the only form two spellings of one number agree on.
func NormalizePhone(raw string) (string, error) {
	digits := onlyDigits(raw)
	switch {
	case len(digits) == 11:
		// (84) 99440-9624, area code plus a nine-digit mobile.
		digits = "55" + digits
	case len(digits) == 13 && strings.HasPrefix(digits, "55"):
		// Already carries the country code.
	case len(digits) >= 8 && len(digits) <= 15:
		// An international number. E.164 allows up to fifteen digits, and a
		// buyer at an international event may legitimately have one.
	default:
		return "", ErrInvalidPhone
	}
	return digits, nil
}

// ageOn is whole years, counting the birthday rather than dividing by 365.25.
func ageOn(birth, today time.Time) int {
	years := today.Year() - birth.Year()
	if today.YearDay() < birth.YearDay() {
		years--
	}
	return years
}

func onlyDigits(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func alphanumeric(value string) bool {
	for _, char := range value {
		isDigit := char >= '0' && char <= '9'
		isLetter := char >= 'A' && char <= 'Z'
		if !isDigit && !isLetter {
			return false
		}
	}
	return true
}

// validCPF checks the two verification digits.
//
// The all-same-digit values (00000000000, 11111111111 …) pass the arithmetic
// and are not real documents, so they are rejected explicitly; they are what
// somebody types to get past a form.
func validCPF(digits string) bool {
	if allSame(digits) {
		return false
	}
	for _, position := range []int{9, 10} {
		sum := 0
		for index := 0; index < position; index++ {
			sum += int(digits[index]-'0') * (position + 1 - index)
		}
		check := (sum * 10) % 11
		if check == 10 {
			check = 0
		}
		if check != int(digits[position]-'0') {
			return false
		}
	}
	return true
}

// validCNPJ checks the two verification digits, with the weights the Receita
// Federal specifies.
func validCNPJ(digits string) bool {
	if allSame(digits) {
		return false
	}
	weights := []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	for _, position := range []int{12, 13} {
		offset := len(weights) - position
		sum := 0
		for index := 0; index < position; index++ {
			sum += int(digits[index]-'0') * weights[offset+index]
		}
		check := sum % 11
		if check < 2 {
			check = 0
		} else {
			check = 11 - check
		}
		if check != int(digits[position]-'0') {
			return false
		}
	}
	return true
}

func allSame(digits string) bool {
	for index := 1; index < len(digits); index++ {
		if digits[index] != digits[0] {
			return false
		}
	}
	return true
}
