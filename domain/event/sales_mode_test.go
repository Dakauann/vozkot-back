package event

import (
	"testing"
	"time"
)

// An event says how it sells, and most events sell by the number.
//
// The default matters more than it looks: every event that predates the field
// was counted, and an organiser who never thinks about the question must end up
// in the majority case rather than in a half-configured seated one.
func TestANewEventSellsByTheNumberUnlessAsked(t *testing.T) {
	now := time.Now()
	draft := Draft{Name: "Festival Aurora", StartsAt: now.Add(time.Hour), Location: validLocation()}

	created, err := New("evt_1", "usr_1", draft, now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if created.SalesMode != SalesCounted {
		t.Errorf("SalesMode = %q, want %q", created.SalesMode, SalesCounted)
	}
	if created.SalesMode.Seated() {
		t.Error("a counted event reports itself as seated")
	}

	draft.SalesMode = SalesSeated
	seated, err := New("evt_2", "usr_1", draft, now)
	if err != nil {
		t.Fatalf("New(seated): %v", err)
	}
	if !seated.SalesMode.Seated() {
		t.Errorf("SalesMode = %q, want it to report as seated", seated.SalesMode)
	}
}

func TestAnInvalidSalesModeIsRefused(t *testing.T) {
	now := time.Now()
	_, err := New("evt_1", "usr_1", Draft{
		Name:      "Festival Aurora",
		StartsAt:  now.Add(time.Hour),
		Location:  validLocation(),
		SalesMode: "reserved",
	}, now)
	if err != ErrInvalidSalesMode {
		t.Fatalf("New() error = %v, want ErrInvalidSalesMode", err)
	}
}

// An edit that says nothing about the mode leaves it alone.
//
// It is a structural choice made once, and there are two callers that rebuild a
// whole Draft from the stored event to change one unrelated thing — the map pin
// is one of them. If an omitted field meant "counted", dragging a pin would
// quietly stop a theatre from selling seats.
func TestAnEditWithoutASalesModeKeepsWhatItHad(t *testing.T) {
	now := time.Now()
	seated, err := New("evt_1", "usr_1", Draft{
		Name:      "Teatro",
		StartsAt:  now.Add(time.Hour),
		Location:  validLocation(),
		SalesMode: SalesSeated,
	}, now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	err = seated.Apply(Draft{
		Name:     "Teatro José de Alencar",
		StartsAt: seated.StartsAt,
		Location: seated.Location,
		Status:   seated.Status,
	}, now)
	if err != nil {
		t.Fatalf("Apply(): %v", err)
	}
	if seated.SalesMode != SalesSeated {
		t.Errorf("SalesMode = %q after an unrelated edit, want %q", seated.SalesMode, SalesSeated)
	}
}

func validLocation() Location {
	return Location{Venue: "Arena Castelão", City: "Fortaleza", UF: "CE"}
}
