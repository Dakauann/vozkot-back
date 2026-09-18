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

// Gender is what a buyer says about themselves, and it is OPTIONAL in a way
// the document is not.
//
// The document exists because a sale cannot lawfully proceed without it. This
// exists because an organiser planning next year's line-up wants to know who
// came, and that is a good reason to ASK and never a reason to require: a buyer
// who does not want to answer must still be able to buy a ticket. Hence
// GenderUndisclosed, which is a real answer and not a missing one, and hence
// the empty value staying valid throughout.
type Gender string

const (
	GenderFemale    Gender = "female"
	GenderMale      Gender = "male"
	GenderNonBinary Gender = "non_binary"
	// GenderOther is for someone whose answer is none of the above and who is
	// willing to say so, which is a different fact from declining to answer.
	GenderOther Gender = "other"
	// GenderUndisclosed is "prefiro não informar", chosen deliberately. A
	// report counts it beside the blanks, because to a count of an audience
	// they mean the same thing, but storing it distinctly is what stops the
	// form asking again every time.
	GenderUndisclosed Gender = "undisclosed"
)

// Genders is the whole set, in the order a form should offer it.
func Genders() []Gender {
	return []Gender{GenderFemale, GenderMale, GenderNonBinary, GenderOther, GenderUndisclosed}
}

// Valid accepts the empty value: gender is optional, and "not answered" is a
// legitimate state of this field rather than a validation failure.
func (g Gender) Valid() bool {
	if g == "" {
		return true
	}
	for _, known := range Genders() {
		if g == known {
			return true
		}
	}
	return false
}

// Known reports whether a report can count this as an answer.
func (g Gender) Known() bool { return g != "" && g != GenderUndisclosed }

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
	ErrUnderage            = errors.New("you must be at least 18 to buy tickets")
	ErrInvalidPhone        = errors.New("phone number is invalid")
	ErrInvalidGender       = errors.New("gender is not one of the supported values")
	// ErrInvalidUF is a two-letter state that is not one of Brazil's 27. A
	// free-text state is the field that turns an audience report into "SP",
	// "sp", "São Paulo" and "Sao Paulo" counted as four places.
	ErrInvalidUF = errors.New("state must be one of Brazil's 27 federal units")
	// ErrDocumentInUse is one document already attached to another account.
	// It is what stops the per-document purchase caps being sidestepped by
	// making a second account with the same CPF.
	ErrDocumentInUse = errors.New("this document is already linked to another account")
)

// MinimumAge is the floor for holding an account that can buy.
//
// Eighteen, which is civil capacity to contract under Código Civil art. 5. It
// is a real check rather than a checkbox: a date of birth that is being
// collected anyway should be used for the one thing it is for.
//
// The floor is eighteen rather than sixteen because an account held by an
// adolescent is adolescent data, and that pulls in LGPD art. 14's best-interest
// duty, GDPR art. 8's consent age and the ECA Digital's guardian-linked-account
// rules for the under-sixteens. A ticket for a minor is bought by an adult on
// their behalf, which is how the door works anyway, and the whole regime stays
// out of the product.
const MinimumAge = 18

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

	// --- what the buyer volunteers, for the organiser's audience report ---
	//
	// None of the three is required and none of them gates a purchase. They
	// exist because an organiser deciding where to take a tour next needs to
	// know who bought this one, and because asking is the only honest way to
	// find out: the alternative is inferring it from an IP address or from a
	// first name, which is both worse data and a worse thing to do.
	//
	// They are sealed at rest like everything else on this struct. Not because
	// a city identifies anybody on its own, but because the report never reads
	// them from here: it reads the coarse snapshot copied onto the order at
	// purchase, so there is no query that encryption costs anything, and a
	// rule of "everything a person told us about themselves is encrypted" is
	// one nobody has to relitigate per field.
	Gender Gender
	City   string
	// UF is the two-letter state code, upper-cased.
	UF string

	PhoneVerifiedAt *time.Time
	CompletedAt     *time.Time
}

// Complete reports whether the legally required block is filled in.
//
// The optional demographic fields deliberately do not count. "Can this account
// buy a ticket" is a question about the document, and answering it with "have
// they told us their city" would put an optional field in front of a sale.
func (p Profile) Complete() bool { return p.CompletedAt != nil }

// AgeOn is the buyer's age in whole years at a given instant, or 0 when there
// is no usable date of birth.
//
// Exported because the order snapshot needs exactly this number and must not
// compute it a second way: an age the profile screen and the audience report
// disagree about is a report nobody trusts.
func (p Profile) AgeOn(now time.Time) int {
	birth, err := time.Parse("2006-01-02", strings.TrimSpace(p.BirthDate))
	if err != nil {
		return 0
	}
	years := ageOn(birth, now.UTC())
	if years < 0 {
		return 0
	}
	return years
}

// PhoneVerified reports whether the number was proven, not merely typed.
func (p Profile) PhoneVerified() bool { return p.PhoneVerifiedAt != nil }

// ProfileDraft is what a buyer supplies.
type ProfileDraft struct {
	DocumentType string
	Document     string
	LegalName    string
	BirthDate    string
	// Gender, City and UF are optional. An empty string means "not answered"
	// and is carried through as one, never defaulted to a value that would
	// make a report count somebody as something they never said.
	Gender string
	City   string
	UF     string
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

	gender := Gender(strings.ToLower(strings.TrimSpace(draft.Gender)))
	if !gender.Valid() {
		return Profile{}, ErrInvalidGender
	}

	// A blank state is fine; a state that is not a state is not. The two are
	// different mistakes and only the second is worth refusing over.
	uf := strings.ToUpper(strings.TrimSpace(draft.UF))
	if uf != "" && !ValidUF(uf) {
		return Profile{}, ErrInvalidUF
	}
	// The city is free text and stays free text: Brazil has 5,570 municipalities
	// and a fixed list of them is a list that is wrong the first time one is
	// created. It is collapsed to single spaces so "Rio  de Janeiro" and
	// "Rio de Janeiro" are one city to a report that groups by it.
	city := strings.Join(strings.Fields(draft.City), " ")
	if len([]rune(city)) > 120 {
		city = string([]rune(city)[:120])
	}

	return Profile{
		DocumentType: documentType,
		Document:     document,
		LegalName:    legalName,
		BirthDate:    birth.Format("2006-01-02"),
		Gender:       gender,
		City:         city,
		UF:           uf,
	}, nil
}

// UFs is Brazil's 27 federal units, alphabetically, which is the order a form
// should offer them in.
var ufs = []string{
	"AC", "AL", "AP", "AM", "BA", "CE", "DF", "ES", "GO", "MA", "MT", "MS",
	"MG", "PA", "PB", "PR", "PE", "PI", "RJ", "RN", "RS", "RO", "RR", "SC",
	"SP", "SE", "TO",
}

// UFs returns a copy of the state list, so a caller cannot reorder the original
// for everybody else.
func UFs() []string { return append([]string(nil), ufs...) }

// ValidUF reports whether an upper-cased two-letter code is a real state.
func ValidUF(uf string) bool {
	for _, known := range ufs {
		if uf == known {
			return true
		}
	}
	return false
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
// ageOn is whole years elapsed, counted by the calendar date.
//
// Month and day rather than YearDay: the ordinal of a date shifts by one
// across a leap year, so comparing ordinals denies somebody born on
// 13 September 2008 on their eighteenth birthday in 2026. Comparing the date
// itself is what a door does, and it has no such day.
func ageOn(birth, today time.Time) int {
	years := today.Year() - birth.Year()
	birthMonth, birthDay := birth.Month(), birth.Day()
	month, day := today.Month(), today.Day()
	if month < birthMonth || (month == birthMonth && day < birthDay) {
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
