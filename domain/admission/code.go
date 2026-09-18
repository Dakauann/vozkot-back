package admission

import (
	"crypto/rand"
	"errors"
	"strings"
)

// The admission code: what a scanner reads and what a doorperson types.
//
// One secret per admission, in one format, rendered two ways. The QR carries
// the bare code and the printed line carries the same characters in groups,
// because a door that cannot scan has to be able to fall back to typing and a
// second code format would be a second thing to keep in sync.
//
// Three properties, in the order they matter:
//
//  1. UNGUESSABLE. Fifty-four bits from crypto/rand. An attacker who knows the
//     format and can ask the door a thousand times a second needs some 10^8
//     years, and the scan endpoint is rate limited besides. It is deliberately
//     not sized to make collisions unlikely: see NewCode for why that is the
//     database's job and not the alphabet's.
//  2. TYPABLE. Crockford's base32 alphabet, which drops I, L, O and U: the
//     first three because they are the characters people confuse with 1 and 0
//     on a printed ticket under bad lighting, and U because dropping it keeps
//     the alphabet from spelling anything. S goes too; see below.
//  3. SELF-CHECKING. One check character, so a mistyped code is refused by
//     arithmetic before it reaches the database. That matters at a door: "that
//     is not a code" is a different conversation from "that code is not
//     valid", and only the first one is the doorperson's typing.
//
// The alphabet is THIRTY-ONE characters, not thirty-two, and the missing one
// is the reason the check character works. See checkCharacter: a weighted sum
// modulo a prime detects every single-character substitution and every
// transposition, and a modulus of 32 provably cannot: with any even weight,
// changing a character by exactly 16 leaves the sum unmoved. Dropping one
// character to reach a prime costs half a bit of entropy and buys a check
// character with no blind spot. S is the one dropped, because 5 is the digit
// it gets confused with.
const (
	// Alphabet is Crockford base32 less S: 31 characters, a prime count. The
	// index of a character IS its value.
	Alphabet = "0123456789ABCDEFGHJKMNPQRTVWXYZ"
	// DataLength is how many random characters a code carries.
	DataLength = 11
	// Length is the whole code: the random part plus its check character.
	Length = DataLength + 1
	// GroupSize is how the printed form is chunked. Twelve characters as
	// three groups of four, which is the shape of every licence key and
	// voucher a person has typed before.
	GroupSize = 4
)

var (
	// ErrMalformedCode is a code that cannot be one: wrong length, a character
	// outside the alphabet, or a failed check character.
	//
	// One error for all three on purpose. Telling a caller which of them
	// failed tells an attacker probing the endpoint how close they are.
	ErrMalformedCode = errors.New("that is not a valid admission code")
)

// Code is a normalised admission code: twelve uppercase characters from
// Alphabet, no separators, check character included and verified.
//
// A distinct type so a raw string from a scanner, a URL or a form cannot be
// passed where a verified code is expected. ParseCode is the only way in.
type Code string

// NewCode mints a fresh code.
//
// Uniqueness is NOT this function's promise. It draws from a space of 31^11, so
// two equal codes are not a thing that happens, but "not a thing that happens"
// is not a guarantee to hang a door on. The guarantee is a UNIQUE index on the
// blind index of the code, and the issuing repository retries on the conflict.
// That is the same division of labour the rest of this system uses: entropy
// makes a value unguessable, a constraint makes it unique.
func NewCode() (Code, error) {
	// One byte per character, rejected and redrawn when it falls outside the
	// largest whole multiple of the alphabet size. Taking the modulo of a raw
	// byte would make the first few characters likelier than the rest: 256 is
	// not a multiple of 31, which costs a fraction of a bit and is the kind of
	// thing that is embarrassing to explain later.
	const limit = 256 - (256 % len(Alphabet))

	builder := strings.Builder{}
	builder.Grow(Length)
	buffer := make([]byte, DataLength)
	for builder.Len() < DataLength {
		if _, err := rand.Read(buffer); err != nil {
			return "", err
		}
		for _, drawn := range buffer {
			if int(drawn) >= limit {
				continue
			}
			builder.WriteByte(Alphabet[int(drawn)%len(Alphabet)])
			if builder.Len() == DataLength {
				break
			}
		}
	}

	data := builder.String()
	return Code(data + string(checkCharacter(data))), nil
}

// ParseCode normalises and verifies whatever a scanner or a form produced.
//
// Forgiving about presentation and strict about content. It accepts the
// grouped form, lower case, stray whitespace and the substitutions a person
// actually makes: O for zero, I or L for one, because a doorperson typing
// from a printed ticket will make exactly those. It rejects anything whose
// check character does not agree, which is what turns a typo into an
// immediate "try again" instead of a database lookup that finds nothing.
func ParseCode(raw string) (Code, error) {
	normalised := strings.Builder{}
	normalised.Grow(Length)
	for _, character := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch character {
		case ' ', '-', '.', '\t', '\n', '\r', '_':
			// Separators a person or a label printer may have added.
			continue
		case 'O':
			character = '0'
		case 'I', 'L':
			character = '1'
		case 'S':
			// Not in the alphabet, and 5 is what it was printed as.
			character = '5'
		}
		if !strings.ContainsRune(Alphabet, character) {
			return "", ErrMalformedCode
		}
		normalised.WriteRune(character)
		if normalised.Len() > Length {
			return "", ErrMalformedCode
		}
	}

	candidate := normalised.String()
	if len(candidate) != Length {
		return "", ErrMalformedCode
	}
	data, check := candidate[:DataLength], candidate[DataLength]
	if check != checkCharacter(data) {
		return "", ErrMalformedCode
	}
	return Code(candidate), nil
}

// Formatted is the code as a ticket prints it: groups of four, hyphenated.
func (c Code) Formatted() string {
	raw := string(c)
	if raw == "" {
		return ""
	}
	groups := make([]string, 0, (len(raw)+GroupSize-1)/GroupSize)
	for start := 0; start < len(raw); start += GroupSize {
		end := start + GroupSize
		if end > len(raw) {
			end = len(raw)
		}
		groups = append(groups, raw[start:end])
	}
	return strings.Join(groups, "-")
}

// String is the bare code, which is what goes into the QR.
//
// Unformatted deliberately: a scanner should hand back exactly the characters
// that were encoded, and the hyphens exist only for human eyes.
func (c Code) String() string { return string(c) }

// checkCharacter is a position-weighted sum over the alphabet, modulo its
// PRIME size.
//
// The two properties, and why they hold:
//
//   - Every single-character substitution is caught. Changing position i by
//     delta moves the sum by w(i)·delta. With a prime modulus and weights well
//     under it, that is zero only when delta is, and delta cannot be zero for
//     a character that changed.
//   - Every transposition is caught, adjacent or not. Swapping positions i and
//     j moves the sum by (w(j) - w(i))·(a - b). Neither factor can be a
//     multiple of the modulus: the weights differ by less than it, and the
//     values differ by less than it, so the product is never zero.
//
// Both arguments need the modulus to be PRIME, which is the whole reason the
// alphabet is 31 characters. A modulus of 32 fails the first property for any
// even weight: a character changed by exactly 16 leaves an even-weighted sum
// unmoved, so half the positions would accept a wrong character. That is not a
// hypothetical: it is what the first version of this function did, and the
// property test above caught it.
//
// It is not a cryptographic check and is not meant to be. Forging a code means
// guessing 54 bits; the check character only ever saves a doorperson a round
// trip to the database when their thumbs slipped.
func checkCharacter(data string) byte {
	sum := 0
	for index := 0; index < len(data); index++ {
		value := strings.IndexByte(Alphabet, data[index])
		if value < 0 {
			// Unreachable for a code this package built, and a deterministic
			// answer is better than a panic on the door's critical path.
			return Alphabet[0]
		}
		sum += (index + 1) * value
	}
	return Alphabet[sum%len(Alphabet)]
}
