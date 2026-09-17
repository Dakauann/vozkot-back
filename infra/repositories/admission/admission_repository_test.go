package admission

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	domain "vozkot/domain/admission"
	"vozkot/infra/testsupport"
)

// Real PostgreSQL, because everything worth testing here is a guarantee the
// database makes: the unique index that stops two codes being equal, and the
// conditional UPDATE that stops two doors admitting one ticket.

type harness struct {
	db         *gorm.DB
	repository *AdmissionRepository
	orderID    string
	eventID    string
	ticketID   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	testsupport.Encryption(t)
	db := testsupport.Database(t)

	orderID := testsupport.Unique("ord")
	eventID := testsupport.Unique("evt")
	ticketID := testsupport.Unique("tkt")
	t.Cleanup(func() { db.Exec("DELETE FROM admissions WHERE order_id = ?", orderID) })

	return &harness{
		db:         db,
		repository: NewAdmissionRepository(db),
		orderID:    orderID,
		eventID:    eventID,
		ticketID:   ticketID,
	}
}

func (h *harness) lines(quantity int) domain.OrderLines {
	return domain.OrderLines{
		OrderID: h.orderID,
		EventID: h.eventID,
		Lines:   []domain.Line{{TicketID: h.ticketID, TicketTitle: "Pista", Quantity: quantity}},
	}
}

func TestIssueForOrderMintsOnePerTicket(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	issued, err := h.repository.IssueForOrder(ctx, h.lines(3))
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}
	if len(issued) != 3 {
		t.Fatalf("issued %d admissions for 3 tickets", len(issued))
	}

	sequences := map[int]bool{}
	codes := map[domain.Code]bool{}
	for _, item := range issued {
		if item.Status != domain.StatusIssued {
			t.Errorf("admission %s is %q, want issued", item.ID, item.Status)
		}
		if item.OrderID != h.orderID || item.EventID != h.eventID || item.TicketID != h.ticketID {
			t.Errorf("admission %s does not name its order, event and tier: %+v", item.ID, item)
		}
		if item.TicketTitle != "Pista" {
			t.Errorf("admission %s lost the tier title: %q", item.ID, item.TicketTitle)
		}
		if _, err := domain.ParseCode(item.Code.String()); err != nil {
			t.Errorf("admission %s has an unparseable code %q: %v", item.ID, item.Code, err)
		}
		if codes[item.Code] {
			t.Errorf("two admissions of one order share the code %q", item.Code)
		}
		codes[item.Code] = true
		sequences[item.Sequence] = true
	}
	// Numbered from one, so a ticket can say "2 de 3".
	for sequence := 1; sequence <= 3; sequence++ {
		if !sequences[sequence] {
			t.Errorf("no admission carries sequence %d", sequence)
		}
	}
}

// A redelivered webhook settles the same order twice. The second settlement
// must not mint a second set of tickets.
func TestIssueForOrderIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.repository.IssueForOrder(ctx, h.lines(2))
	if err != nil {
		t.Fatalf("first IssueForOrder(): %v", err)
	}
	second, err := h.repository.IssueForOrder(ctx, h.lines(2))
	if err != nil {
		t.Fatalf("second IssueForOrder(): %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("the second issue created %d admissions; a redelivery must create none", len(second))
	}

	var stored int64
	if err := h.db.Raw("SELECT COUNT(*) FROM admissions WHERE order_id = ?", h.orderID).Scan(&stored).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if stored != int64(len(first)) {
		t.Fatalf("%d admissions stored after two issues, want %d", stored, len(first))
	}
}

// Two settlements racing. Exactly one of them may create the tickets, and the
// other must be told it created none rather than failing the payment.
func TestConcurrentIssuesCreateOneSet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const racers = 8
	var wait sync.WaitGroup
	created := make([]int, racers)
	failures := make([]error, racers)

	wait.Add(racers)
	for index := 0; index < racers; index++ {
		go func(slot int) {
			defer wait.Done()
			issued, err := h.repository.IssueForOrder(ctx, h.lines(2))
			created[slot] = len(issued)
			failures[slot] = err
		}(index)
	}
	wait.Wait()

	winners := 0
	for slot, count := range created {
		if failures[slot] != nil {
			t.Fatalf("racer %d failed: %v", slot, failures[slot])
		}
		if count > 0 {
			winners++
			if count != 2 {
				t.Errorf("racer %d created %d admissions, want 2", slot, count)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers created admissions, want exactly 1", winners)
	}

	var stored int64
	if err := h.db.Raw("SELECT COUNT(*) FROM admissions WHERE order_id = ?", h.orderID).Scan(&stored).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if stored != 2 {
		t.Fatalf("%d admissions stored, want 2", stored)
	}
}

// The code must not be readable out of the table. A dump of this database must
// not be a stack of working tickets.
func TestTheCodeIsSealedAtRest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	issued, err := h.repository.IssueForOrder(ctx, h.lines(1))
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}
	code := issued[0].Code.String()

	// Row().Scan rather than GORM's Scan: the column is bytea and GORM's
	// convenience path tries to convert it to a single uint8.
	var raw []byte
	if err := h.db.Raw("SELECT code FROM admissions WHERE id = ?", issued[0].ID).
		Row().Scan(&raw); err != nil {
		t.Fatalf("read the raw column: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("the code column is empty; nothing was stored")
	}
	if strings.Contains(string(raw), code) {
		t.Fatalf("the code %q is readable in the table", code)
	}

	// And it must not be findable by its plaintext either, which is what the
	// blind index exists to make possible without storing it.
	var byPlaintext int64
	if err := h.db.Raw("SELECT COUNT(*) FROM admissions WHERE code::text LIKE ?", "%"+code+"%").
		Scan(&byPlaintext).Error; err != nil {
		t.Fatalf("search by plaintext: %v", err)
	}
	if byPlaintext != 0 {
		t.Fatal("an admission is findable by its plaintext code")
	}

	// The holder still gets it back, which is the whole reason it is sealed
	// rather than hashed.
	found, err := h.repository.FindByCode(ctx, issued[0].Code)
	if err != nil {
		t.Fatalf("FindByCode(): %v", err)
	}
	if found.Code != issued[0].Code {
		t.Fatalf("FindByCode() returned the code %q, want %q", found.Code, issued[0].Code)
	}
}

func TestFindByCodeRefusesAnUnknownCode(t *testing.T) {
	h := newHarness(t)

	unknown, err := domain.NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	if _, err := h.repository.FindByCode(context.Background(), unknown); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindByCode() on an unissued code = %v, want ErrNotFound", err)
	}
}

// The property the whole door depends on: one ticket admits one person, even
// when two scanners read it in the same instant.
func TestAdmitLetsExactlyOneScanThrough(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	issued, err := h.repository.IssueForOrder(ctx, h.lines(1))
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}
	id := issued[0].ID

	const scanners = 16
	var wait sync.WaitGroup
	admitted := make([]bool, scanners)
	failures := make([]error, scanners)

	wait.Add(scanners)
	for index := 0; index < scanners; index++ {
		go func(slot int) {
			defer wait.Done()
			ok, err := h.repository.Admit(ctx, id, testsupport.Unique("usr"))
			admitted[slot] = ok
			failures[slot] = err
		}(index)
	}
	wait.Wait()

	winners := 0
	for slot, ok := range admitted {
		if failures[slot] != nil {
			t.Fatalf("scanner %d failed: %v", slot, failures[slot])
		}
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d simultaneous scans were admitted; exactly one may be", winners, scanners)
	}

	found, err := h.repository.FindByCode(ctx, issued[0].Code)
	if err != nil {
		t.Fatalf("FindByCode(): %v", err)
	}
	if found.Status != domain.StatusAdmitted {
		t.Fatalf("status after admitting = %q", found.Status)
	}
	if found.AdmittedAt == nil || found.AdmittedBy == "" {
		t.Fatalf("the admission does not record when or by whom: %v / %q", found.AdmittedAt, found.AdmittedBy)
	}
}

func TestAdmitRefusesASpentAdmission(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	issued, err := h.repository.IssueForOrder(ctx, h.lines(1))
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}

	ok, err := h.repository.Admit(ctx, issued[0].ID, "usr_door")
	if err != nil || !ok {
		t.Fatalf("the first Admit() = %v, %v; want true, nil", ok, err)
	}
	ok, err = h.repository.Admit(ctx, issued[0].ID, "usr_door")
	if err != nil {
		t.Fatalf("the second Admit() errored: %v", err)
	}
	if ok {
		t.Fatal("the second Admit() reported success")
	}

	// A void admission is not admissible either.
	void, err := h.repository.IssueForOrder(ctx, domain.OrderLines{
		OrderID: testsupport.Unique("ord"),
		EventID: h.eventID,
		Lines:   []domain.Line{{TicketID: h.ticketID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}
	t.Cleanup(func() { h.db.Exec("DELETE FROM admissions WHERE order_id = ?", void[0].OrderID) })

	if _, err := h.repository.VoidForOrder(ctx, void[0].OrderID); err != nil {
		t.Fatalf("VoidForOrder(): %v", err)
	}
	if ok, err := h.repository.Admit(ctx, void[0].ID, "usr_door"); err != nil || ok {
		t.Fatalf("Admit() on a void admission = %v, %v; want false, nil", ok, err)
	}
}

func TestVoidForOrderIsIdempotentAndKeepsAttendance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	issued, err := h.repository.IssueForOrder(ctx, h.lines(3))
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}
	// One of them came in before the refund.
	if ok, err := h.repository.Admit(ctx, issued[0].ID, "usr_door"); err != nil || !ok {
		t.Fatalf("Admit(): %v, %v", ok, err)
	}

	voided, err := h.repository.VoidForOrder(ctx, h.orderID)
	if err != nil {
		t.Fatalf("VoidForOrder(): %v", err)
	}
	if voided != 3 {
		t.Fatalf("voided %d admissions, want 3", voided)
	}

	again, err := h.repository.VoidForOrder(ctx, h.orderID)
	if err != nil {
		t.Fatalf("the second VoidForOrder(): %v", err)
	}
	if again != 0 {
		t.Fatalf("the second void changed %d rows, want 0", again)
	}

	// The person who came in still came in.
	all, err := h.repository.ListByOrder(ctx, h.orderID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	attended := 0
	for _, item := range all {
		if item.Status != domain.StatusVoid {
			t.Errorf("admission %s is %q after a refund, want void", item.ID, item.Status)
		}
		if item.AdmittedAt != nil {
			attended++
		}
	}
	if attended != 1 {
		t.Fatalf("%d admissions record an entry, want 1; a refund must not erase who attended", attended)
	}
}

func TestListByOrderAndCount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.repository.IssueForOrder(ctx, h.lines(4)); err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}

	listed, err := h.repository.ListByOrder(ctx, h.orderID)
	if err != nil {
		t.Fatalf("ListByOrder(): %v", err)
	}
	if len(listed) != 4 {
		t.Fatalf("ListByOrder() returned %d, want 4", len(listed))
	}
	// Ordered by sequence so a holder's four tickets read 1, 2, 3, 4.
	for index, item := range listed {
		if item.Sequence != index+1 {
			t.Fatalf("position %d carries sequence %d", index, item.Sequence)
		}
		if item.Code == "" {
			t.Fatalf("admission %s came back without its code; the holder cannot show it", item.ID)
		}
	}

	if ok, err := h.repository.Admit(ctx, listed[0].ID, "usr_door"); err != nil || !ok {
		t.Fatalf("Admit(): %v, %v", ok, err)
	}
	issued, admitted, err := h.repository.CountAdmitted(ctx, h.eventID)
	if err != nil {
		t.Fatalf("CountAdmitted(): %v", err)
	}
	if issued != 3 || admitted != 1 {
		t.Fatalf("CountAdmitted() = %d issued, %d admitted; want 3 and 1", issued, admitted)
	}
}
