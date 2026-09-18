package admission

import (
	"strings"
	"testing"
)

// The code is the whole security of the door, so these are the properties it
// has to hold rather than an example or two of it working.

func TestNewCodeShape(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}

	if len(code) != Length {
		t.Fatalf("code %q is %d characters, want %d", code, len(code), Length)
	}
	for index, character := range string(code) {
		if !strings.ContainsRune(Alphabet, character) {
			t.Fatalf("code %q has %q at %d, which is outside the alphabet", code, character, index)
		}
	}
	// The ambiguous characters must never be minted, or the printed fallback
	// becomes guesswork.
	for _, banned := range "ILOUS" {
		if strings.ContainsRune(string(code), banned) {
			t.Fatalf("code %q contains %q, which a person cannot read off a ticket reliably", code, banned)
		}
	}
	if _, err := ParseCode(string(code)); err != nil {
		t.Fatalf("a freshly minted code did not parse: %v", err)
	}
}

// Never the same twice. Not proof of uniqueness, that is the database's job,
// but a generator that repeats itself within ten thousand draws is broken in a
// way no constraint would hide.
func TestNewCodeDoesNotRepeat(t *testing.T) {
	const draws = 10_000
	seen := make(map[Code]struct{}, draws)
	for index := 0; index < draws; index++ {
		code, err := NewCode()
		if err != nil {
			t.Fatalf("NewCode() at %d: %v", index, err)
		}
		if _, repeated := seen[code]; repeated {
			t.Fatalf("NewCode() produced %q twice in %d draws", code, draws)
		}
		seen[code] = struct{}{}
	}
}

// Every character position must actually vary. A generator that got its
// slicing wrong could hold one position constant and still pass every test
// above while losing five bits.
func TestNewCodeUsesTheWholeAlphabetInEveryPosition(t *testing.T) {
	const draws = 3_000
	perPosition := make([]map[byte]struct{}, DataLength)
	for index := range perPosition {
		perPosition[index] = map[byte]struct{}{}
	}

	for count := 0; count < draws; count++ {
		code, err := NewCode()
		if err != nil {
			t.Fatalf("NewCode(): %v", err)
		}
		for index := 0; index < DataLength; index++ {
			perPosition[index][code[index]] = struct{}{}
		}
	}

	// With 3000 draws each position should see nearly all 32 characters; 24 is
	// a floor loose enough never to flake and tight enough to catch a position
	// that is stuck or drawn from a truncated alphabet.
	for index, distinct := range perPosition {
		if len(distinct) < 24 {
			t.Errorf("position %d only ever held %d distinct characters in %d draws", index, len(distinct), draws)
		}
	}
}

func TestParseCodeAcceptsWhatAPersonTypes(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	canonical := string(code)

	// Every one of these is something a doorperson or a scanner will actually
	// produce from the same printed ticket.
	variants := map[string]string{
		"canonical":      canonical,
		"grouped":        code.Formatted(),
		"lower case":     strings.ToLower(canonical),
		"spaced":         strings.Join([]string{canonical[:4], canonical[4:8], canonical[8:]}, " "),
		"leading space":  "  " + canonical,
		"trailing space": canonical + "\n",
		"underscored":    strings.Join([]string{canonical[:6], canonical[6:]}, "_"),
	}
	for name, variant := range variants {
		parsed, err := ParseCode(variant)
		if err != nil {
			t.Errorf("%s (%q) did not parse: %v", name, variant, err)
			continue
		}
		if parsed != code {
			t.Errorf("%s parsed to %q, want %q", name, parsed, code)
		}
	}
}

// O for zero and I or L for one are the substitutions people make off a
// printed label, and the alphabet excludes those characters precisely so the
// mapping is unambiguous.
func TestParseCodeForgivesTheAmbiguousCharacters(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	canonical := string(code)

	for substitute, digit := range map[string]string{"O": "0", "I": "1", "L": "1"} {
		mistyped := strings.ReplaceAll(canonical, digit, substitute)
		if mistyped == canonical {
			continue // this code has no such digit
		}
		parsed, err := ParseCode(mistyped)
		if err != nil {
			t.Errorf("%q for %q did not parse: %v", substitute, digit, err)
			continue
		}
		if parsed != code {
			t.Errorf("%q for %q parsed to %q, want %q", substitute, digit, parsed, code)
		}
	}
}

func TestParseCodeRejectsRubbish(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	canonical := string(code)

	cases := map[string]string{
		"empty":            "",
		"too short":        canonical[:Length-1],
		"too long":         canonical + "0",
		"outside alphabet": canonical[:Length-1] + "!",
		"a sentence":       "let me in please",
		// A character the alphabet excludes must not be silently accepted just
		// because it looks like one that is in it.
		"contains U": strings.Repeat("U", Length),
	}
	for name, raw := range cases {
		if _, err := ParseCode(raw); err == nil {
			t.Errorf("%s (%q) was accepted", name, raw)
		}
	}
}

// The check character earns its place here: a typo has to be refused by
// arithmetic, before anything touches the database.
func TestTheCheckCharacterCatchesTypos(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	canonical := string(code)

	t.Run("every single-character substitution", func(t *testing.T) {
		for position := 0; position < Length; position++ {
			for _, replacement := range Alphabet {
				if byte(replacement) == canonical[position] {
					continue
				}
				mistyped := canonical[:position] + string(replacement) + canonical[position+1:]
				if _, err := ParseCode(mistyped); err == nil {
					t.Fatalf("%q (position %d changed to %q) was accepted as valid", mistyped, position, replacement)
				}
			}
		}
	})

	// EVERY transposition, not only adjacent ones. The prime modulus buys the
	// stronger property, so the test asserts the stronger property: with a
	// modulus of 32 this loop is what fails.
	t.Run("every transposition", func(t *testing.T) {
		for first := 0; first < Length; first++ {
			for second := first + 1; second < Length; second++ {
				if canonical[first] == canonical[second] {
					continue // swapping equal characters changes nothing
				}
				swapped := []byte(canonical)
				swapped[first], swapped[second] = swapped[second], swapped[first]
				if _, err := ParseCode(string(swapped)); err == nil {
					t.Fatalf("%q (positions %d and %d swapped) was accepted as valid", swapped, first, second)
				}
			}
		}
	})
}

// Run the two properties above across many codes, not just one, so a check
// character that happens to work for a particular code cannot pass.
func TestTheCheckCharacterHoldsAcrossManyCodes(t *testing.T) {
	for draw := 0; draw < 200; draw++ {
		code, err := NewCode()
		if err != nil {
			t.Fatalf("NewCode(): %v", err)
		}
		canonical := string(code)
		for position := 0; position < Length; position++ {
			next := Alphabet[(strings.IndexByte(Alphabet, canonical[position])+1)%len(Alphabet)]
			mistyped := canonical[:position] + string(next) + canonical[position+1:]
			if _, err := ParseCode(mistyped); err == nil {
				t.Fatalf("draw %d: %q was accepted", draw, mistyped)
			}
		}
	}
}

func TestFormattedIsGroupedAndReversible(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}

	formatted := code.Formatted()
	if want := 3; strings.Count(formatted, "-") != want-1 {
		t.Fatalf("Formatted() = %q, want %d groups", formatted, want)
	}
	for _, group := range strings.Split(formatted, "-") {
		if len(group) != GroupSize {
			t.Fatalf("Formatted() = %q has a group of %d, want %d", formatted, len(group), GroupSize)
		}
	}
	// The QR carries the bare characters, and the printed line the grouped
	// ones. Both have to come back to the same code.
	if parsed, err := ParseCode(formatted); err != nil || parsed != code {
		t.Fatalf("ParseCode(Formatted()) = %q, %v; want %q", parsed, err, code)
	}
	if code.String() != string(code) {
		t.Fatalf("String() = %q, want the bare code %q", code.String(), string(code))
	}
	if strings.Contains(code.String(), "-") {
		t.Fatal("the QR payload contains a hyphen; a scanner should return exactly the encoded characters")
	}
}
