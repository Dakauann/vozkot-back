package admission

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 16, 21, 0, 0, 0, time.UTC)

func draft() Draft {
	return Draft{
		OrderID:     "ord_1",
		EventID:     "evt_1",
		TicketID:    "tkt_1",
		TicketTitle: "Pista",
		Sequence:    1,
	}
}

func TestNewIssuesAUsableAdmission(t *testing.T) {
	item, err := New("adm_1", draft(), now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	if item.Status != StatusIssued || !item.Usable() {
		t.Fatalf("a fresh admission is %q and usable=%v", item.Status, item.Usable())
	}
	if _, err := ParseCode(item.Code.String()); err != nil {
		t.Fatalf("the issued code does not parse: %v", err)
	}
	if item.AdmittedAt != nil || item.VoidedAt != nil {
		t.Fatal("a fresh admission already carries a timestamp")
	}
	if !item.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", item.CreatedAt, now)
	}
}

// Two admissions of the same order line must never share a code. This is the
// property a group of three depends on.
func TestEveryAdmissionGetsItsOwnCode(t *testing.T) {
	seen := map[Code]struct{}{}
	for sequence := 1; sequence <= 50; sequence++ {
		input := draft()
		input.Sequence = sequence
		item, err := New("adm", input, now)
		if err != nil {
			t.Fatalf("New(): %v", err)
		}
		if _, repeated := seen[item.Code]; repeated {
			t.Fatalf("sequence %d reused the code %q", sequence, item.Code)
		}
		seen[item.Code] = struct{}{}
	}
}

func TestNewRefusesAnAdmissionThatNamesNothing(t *testing.T) {
	cases := map[string]func(*Draft){
		"no order": func(d *Draft) { d.OrderID = " " },
		"no event": func(d *Draft) { d.EventID = "" },
		"no tier":  func(d *Draft) { d.TicketID = "" },
	}
	for name, break_ := range cases {
		input := draft()
		break_(&input)
		if _, err := New("adm_1", input, now); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// A missing sequence is not a failure — one admission is the first one.
	input := draft()
	input.Sequence = 0
	item, err := New("adm_1", input, now)
	if err != nil {
		t.Fatalf("New() with no sequence: %v", err)
	}
	if item.Sequence != 1 {
		t.Fatalf("Sequence = %d, want 1", item.Sequence)
	}
}

// Single use is the whole point: the first scan admits, the second refuses and
// says why.
func TestAdmitIsSingleUse(t *testing.T) {
	item, err := New("adm_1", draft(), now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	if err := item.Admit("usr_door", now); err != nil {
		t.Fatalf("the first scan was refused: %v", err)
	}
	if item.Status != StatusAdmitted {
		t.Fatalf("status after admitting = %q", item.Status)
	}
	if item.AdmittedAt == nil || !item.AdmittedAt.Equal(now) {
		t.Fatalf("AdmittedAt = %v, want %v", item.AdmittedAt, now)
	}
	if item.AdmittedBy != "usr_door" {
		t.Fatalf("AdmittedBy = %q, want the scanning account", item.AdmittedBy)
	}
	if item.Usable() {
		t.Fatal("an admitted admission still reports itself usable")
	}

	later := now.Add(time.Minute)
	if err := item.Admit("usr_other", later); !errors.Is(err, ErrAlreadyAdmitted) {
		t.Fatalf("the second scan returned %v, want ErrAlreadyAdmitted", err)
	}
	// The record of the FIRST entry must survive the second attempt, because
	// that timestamp is what the door tells the person in front of them.
	if !item.AdmittedAt.Equal(now) || item.AdmittedBy != "usr_door" {
		t.Fatalf("a rejected second scan overwrote the first: %v by %q", item.AdmittedAt, item.AdmittedBy)
	}
}

func TestAVoidAdmissionCannotBeAdmitted(t *testing.T) {
	item, err := New("adm_1", draft(), now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if !item.Void(now) {
		t.Fatal("Void() on an issued admission reported no change")
	}
	if item.Status != StatusVoid || item.VoidedAt == nil {
		t.Fatalf("status = %q, voidedAt = %v", item.Status, item.VoidedAt)
	}
	if err := item.Admit("usr_door", now); !errors.Is(err, ErrVoid) {
		t.Fatalf("Admit() on a void admission = %v, want ErrVoid", err)
	}
}

// Voiding is idempotent because a refund can settle twice.
func TestVoidIsIdempotent(t *testing.T) {
	item, err := New("adm_1", draft(), now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if !item.Void(now) {
		t.Fatal("the first Void() reported no change")
	}
	firstVoidedAt := *item.VoidedAt

	if item.Void(now.Add(time.Hour)) {
		t.Fatal("the second Void() reported a change")
	}
	if !item.VoidedAt.Equal(firstVoidedAt) {
		t.Fatalf("the second Void() moved VoidedAt to %v", item.VoidedAt)
	}
}

// Somebody who came in and was refunded afterwards attended AND was refunded.
// Both facts are true and the report needs both.
func TestVoidingAnAdmittedAdmissionKeepsTheAttendance(t *testing.T) {
	item, err := New("adm_1", draft(), now)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if err := item.Admit("usr_door", now); err != nil {
		t.Fatalf("Admit(): %v", err)
	}

	if !item.Void(now.Add(24 * time.Hour)) {
		t.Fatal("Void() on an admitted admission reported no change")
	}
	if item.Status != StatusVoid {
		t.Fatalf("status = %q, want void", item.Status)
	}
	if item.AdmittedAt == nil || !item.AdmittedAt.Equal(now) {
		t.Fatalf("voiding erased the attendance: AdmittedAt = %v", item.AdmittedAt)
	}
	if item.AdmittedBy != "usr_door" {
		t.Fatalf("voiding erased who scanned it: %q", item.AdmittedBy)
	}
}

func TestStatusSpent(t *testing.T) {
	for status, spent := range map[Status]bool{
		StatusIssued:   false,
		StatusAdmitted: true,
		StatusVoid:     true,
	} {
		if got := status.Spent(); got != spent {
			t.Errorf("%q.Spent() = %v, want %v", status, got, spent)
		}
		if !status.Valid() {
			t.Errorf("%q is not Valid()", status)
		}
	}
	if Status("smuggled").Valid() {
		t.Error("an unknown status reported itself valid")
	}
}
