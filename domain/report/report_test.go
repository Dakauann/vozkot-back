package report

import "testing"

// Every band must be reachable and the boundaries must land where the labels
// say they do. An off-by-one here silently moves people between bands in every
// report the product will ever print.
func TestBracketBoundaries(t *testing.T) {
	cases := []struct {
		age  int
		want AgeBracket
	}{
		{0, AgeUnknown},
		{-4, AgeUnknown},
		{16, AgeUpTo18},
		{18, AgeUpTo18},
		{19, Age19To23},
		{23, Age19To23},
		{24, Age24To28},
		{28, Age24To28},
		{29, Age29To33},
		{33, Age29To33},
		{34, Age34To38},
		{38, Age34To38},
		{39, Age39To43},
		{43, Age39To43},
		{44, Age44To48},
		{48, Age44To48},
		{49, Age49To53},
		{53, Age49To53},
		{54, Age54To58},
		{58, Age54To58},
		{59, Age59Plus},
		{104, Age59Plus},
	}
	for _, testCase := range cases {
		if got := BracketFor(testCase.age); got != testCase.want {
			t.Errorf("BracketFor(%d) = %q, want %q", testCase.age, got, testCase.want)
		}
	}
}

// A missing date of birth must not be counted as a teenager. Treating zero as
// "up to 18" would invent an audience the organiser does not have.
func TestAMissingAgeIsUnknownAndNotTheYoungestBand(t *testing.T) {
	if BracketFor(0) == AgeUpTo18 {
		t.Fatal("a buyer with no date of birth was counted as 18 or under")
	}
}

// Every bracket BracketFor can return has to be in the list a table renders,
// or a real row would have nowhere to go.
func TestEveryReachableBracketIsListed(t *testing.T) {
	listed := make(map[AgeBracket]bool, len(AgeBrackets()))
	for _, bracket := range AgeBrackets() {
		if listed[bracket] {
			t.Fatalf("%q appears twice in AgeBrackets()", bracket)
		}
		listed[bracket] = true
	}
	for age := -1; age <= 120; age++ {
		if !listed[BracketFor(age)] {
			t.Fatalf("BracketFor(%d) returned %q, which AgeBrackets() does not list",
				age, BracketFor(age))
		}
	}
}

func TestNormalizeClampsAPage(t *testing.T) {
	if got := (AttendeeFilter{}).Normalize().Limit; got != DefaultPageSize {
		t.Fatalf("default limit = %d, want %d", got, DefaultPageSize)
	}
	if got := (AttendeeFilter{Limit: MaxPageSize + 500}).Normalize().Limit; got != MaxPageSize {
		t.Fatalf("limit was not clamped: %d", got)
	}
	if got := (AttendeeFilter{Offset: -10}).Normalize().Offset; got != 0 {
		t.Fatalf("negative offset survived: %d", got)
	}
}
