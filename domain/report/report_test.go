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

// The ticket médio, asked the two ways an organiser means it.
func TestAverageSpendIsReportedPerOrderAndPerAdmission(t *testing.T) {
	// One buyer taking eight tickets and one taking one: the average ORDER is
	// high and the average TICKET is ordinary, and confusing them is how a tier
	// gets repriced on the wrong evidence.
	totals := Totals{Orders: 2, Tickets: 9, NetCents: 90_00}
	if got := totals.AverageOrderCents(); got != 45_00 {
		t.Errorf("average order = %d, want 4500", got)
	}
	if got := totals.AverageTicketCents(); got != 10_00 {
		t.Errorf("average ticket = %d, want 1000", got)
	}
}

func TestAverageSpendRoundsHalfUpAndNeverDividesByZero(t *testing.T) {
	// 100 / 3 = 33.33 -> 33; 101 / 2 = 50.5 -> 51.
	if got := (Totals{Orders: 3, NetCents: 100}).AverageOrderCents(); got != 33 {
		t.Errorf("rounding down = %d, want 33", got)
	}
	if got := (Totals{Orders: 2, NetCents: 101}).AverageOrderCents(); got != 51 {
		t.Errorf("rounding half up = %d, want 51", got)
	}
	// An event that sold nothing has no average, and reporting one would be
	// inventing a number. Zero, never a panic.
	empty := Totals{}
	if empty.AverageOrderCents() != 0 || empty.AverageTicketCents() != 0 {
		t.Errorf("an empty report produced an average: %+v", empty)
	}
}

// A report that covered the whole platform because an id arrived empty is the
// one bug here that leaks every organiser's revenue.
func TestOnlyAScopeNamingExactlyOneSubjectIsValid(t *testing.T) {
	if !EventScope("evt_1").Valid() || !OrganiserScope("usr_1").Valid() {
		t.Fatal("a scope naming one subject was refused")
	}
	for name, scope := range map[string]Scope{
		"empty":    {},
		"both set": {EventID: "evt_1", OrganiserID: "usr_1"},
	} {
		if scope.Valid() {
			t.Errorf("%s scope was accepted", name)
		}
	}
}
