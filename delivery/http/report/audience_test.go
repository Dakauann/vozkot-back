package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	authdomain "vozkot/domain/auth"
	"vozkot/domain/pricing"
	ticketdomain "vozkot/domain/ticket"
	userdomain "vozkot/domain/user"
	eventRepository "vozkot/infra/repositories/event"
	reportRepository "vozkot/infra/repositories/report"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
	usecase "vozkot/usecases/report"
)

// Who is allowed to see our commission, tested at the wire.
//
// The buyer must see the service fee: Decreto 7.962/2013 art. 2, IV-V requires
// the charge to be disclosed before payment and STJ REsp 1.632.928 makes that
// disclosure the compliance obligation. The organiser must NOT: what they are
// owed is the face value they priced, and our margin is not their business.
//
// Those two requirements point in opposite directions through the same
// database columns, which is why this is tested on the response BYTES rather
// than on struct fields. A field added back to any nested response type — a
// slice, a day, a totals block, an attendee row — would make the substring
// reappear, and no amount of remembering to update an assertion is needed for
// this test to catch it.

type harness struct {
	db      *gorm.DB
	mux     *http.ServeMux
	eventID string
	ownerID string
	// netCents is the organiser's share of the seeded order and feeCents is
	// ours. The test asserts the first is reported and the second is not.
	netCents int64
	feeCents int64
}

// newHarness seeds one paid order carrying a real service fee.
func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)
	ctx := context.Background()

	fee, err := pricing.PlatformFee()
	if err != nil {
		t.Fatalf("PlatformFee(): %v", err)
	}

	ownerID := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Organizador', ?, 'x', 'user', 0, NOW(), NOW())`,
		ownerID, ownerID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	eventID := testsupport.SeedEvent(t, db, ownerID)

	const unitFace = 24_000
	const quantity = 2
	unitFee := fee.On(unitFace)
	face := int64(unitFace * quantity)
	feeTotal := unitFee * quantity
	if feeTotal == 0 {
		t.Fatal("the platform fee is zero; this test cannot tell a hidden fee from an absent one")
	}

	orderID := testsupport.Unique("ord")
	buyerID := testsupport.Unique("usr")

	// A real tier, because order_items carries a foreign key to it.
	tier, err := ticketdomain.New(testsupport.Unique("tkt"), ownerID, ticketdomain.Draft{
		EventID:    eventID,
		Title:      "Pista",
		PriceCents: unitFace,
		Quantity:   50,
		Status:     ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build tier: %v", err)
	}
	if err := ticketRepository.NewTicketRepository(db).Create(ctx, tier); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	ticketID := tier.ID
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Maria Souza', ?, 'x', 'user', 0, NOW(), NOW())`,
		buyerID, buyerID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed buyer: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO orders (
			id, event_id, buyer_id, buyer_name, buyer_email, buyer_document,
			buyer_gender, buyer_age_years, buyer_city, buyer_uf,
			subtotal_cents, service_fee_cents, total_cents, currency,
			refund_policy_version, status, hold_expires_at, confirmed,
			paid_at, created_at, updated_at
		) VALUES (
			?, ?, ?, 'Maria Souza', ?, '12345678909',
			'female', 31, 'Natal', 'RN',
			?, ?, ?, 'BRL',
			1, 'paid', NOW(), true,
			NOW(), NOW(), NOW()
		)`,
		orderID, eventID, buyerID, buyerID+"@vozkot.test",
		face, feeTotal, face+feeTotal).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO order_items (
			id, order_id, ticket_id, ticket_title, quantity,
			unit_price_cents, unit_fee_cents, total_cents, fee_cents, created_at
		) VALUES (?, ?, ?, 'Pista', ?, ?, ?, ?, ?, NOW())`,
		testsupport.Unique("oit"), orderID, ticketID, quantity,
		int64(unitFace), unitFee, face, feeTotal).Error; err != nil {
		t.Fatalf("seed order item: %v", err)
	}

	t.Cleanup(func() {
		db.Exec("DELETE FROM order_items WHERE order_id = ?", orderID)
		db.Exec("DELETE FROM orders WHERE id = ?", orderID)
		db.Exec("DELETE FROM tickets WHERE id = ?", ticketID)
		db.Exec("DELETE FROM users WHERE id IN (?, ?)", ownerID, buyerID)
	})

	service := usecase.NewService(
		reportRepository.NewReportRepository(db),
		eventRepository.NewEventRepository(db),
	)
	mux := http.NewServeMux()
	NewHandler(service).Register(mux)

	// Sanity: the seeding itself must have produced a report with money in it,
	// or every "the fee is absent" assertion below would pass trivially.
	sales, err := service.Sales(ctx, eventID, authdomain.Actor{ID: ownerID, Role: userdomain.RoleUser})
	if err != nil {
		t.Fatalf("seeded event produced no report: %v", err)
	}
	if sales.Totals.NetCents != face {
		t.Fatalf("seeded net = %d, want %d; the fixture is wrong, not the code",
			sales.Totals.NetCents, face)
	}

	return &harness{
		db: db, mux: mux, eventID: eventID, ownerID: ownerID,
		netCents: face, feeCents: feeTotal,
	}
}

func (h *harness) get(t *testing.T, path, role string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(authdomain.WithClaims(request.Context(), &authdomain.Claims{
		UserID: h.ownerID, Role: role,
	}))
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// forbiddenMoneyKeys are the JSON names that would expose our side of the
// split. Checked as substrings of the whole body, so a field buried in any
// nested object is caught.
var forbiddenMoneyKeys = []string{"feeCents", "grossCents", "paidCents", "serviceFeeCents", "totalCents"}

func assertNoPlatformMoney(t *testing.T, where, body string) {
	t.Helper()
	for _, key := range forbiddenMoneyKeys {
		if strings.Contains(body, key) {
			t.Errorf("%s exposes %q to the organiser; that is our commission or the gross it can be derived from", where, key)
		}
	}
}

// The sales dashboard: the organiser's earnings, and nothing of ours.
func TestSalesReportHidesThePlatformCommission(t *testing.T) {
	h := newHarness(t)

	recorder := h.get(t, "/api/v1/events/"+h.eventID+"/report", "user")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET report = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	assertNoPlatformMoney(t, "the sales report", body)

	// And the number that IS there has to be the organiser's, not a zero left
	// behind by hiding something.
	var envelope SalesEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if envelope.Data.Totals.NetCents != h.netCents {
		t.Errorf("netCents = %d, want the organiser's %d",
			envelope.Data.Totals.NetCents, h.netCents)
	}

	// The gross must not be recoverable by adding the breakdowns up either.
	for name, slices := range map[string][]SliceResponse{
		"byGender": envelope.Data.ByGender,
		"byUF":     envelope.Data.ByUF,
		"byCity":   envelope.Data.ByCity,
		"byTier":   envelope.Data.ByTier,
	} {
		if len(slices) == 0 {
			t.Errorf("%s came back empty; the fixture should have produced a bucket", name)
			continue
		}
		total := int64(0)
		for _, slice := range slices {
			total += slice.NetCents
		}
		if total != h.netCents {
			t.Errorf("%s sums to %d, want the organiser's %d", name, total, h.netCents)
		}
	}

	if len(envelope.Data.ByDay) == 0 {
		t.Error("byDay came back empty; the seeded order is paid and should appear on a day")
	}
	for _, day := range envelope.Data.ByDay {
		if day.NetCents != h.netCents {
			t.Errorf("day %s netCents = %d, want %d", day.Day, day.NetCents, h.netCents)
		}
	}
}

// An administrator is us. They reach the same route, and it stays net —
// deliberately, because this report is the organiser's report whoever opens it.
// Our revenue is not reported here at all; it lives on orders.service_fee_cents.
func TestSalesReportIsNetForAdministratorsToo(t *testing.T) {
	h := newHarness(t)

	recorder := h.get(t, "/api/v1/events/"+h.eventID+"/report", "admin")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET report as admin = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	assertNoPlatformMoney(t, "the sales report read by an admin", recorder.Body.String())
}

// The attendee list, which is the other thing an organiser reads.
func TestAttendeeListHidesThePlatformCommission(t *testing.T) {
	h := newHarness(t)

	recorder := h.get(t, "/api/v1/events/"+h.eventID+"/attendees", "user")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET attendees = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	assertNoPlatformMoney(t, "the attendee list", body)

	var envelope AttendeeListEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode attendees: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("no attendees came back; the seeded paid order should be one")
	}
	row := envelope.Data[0]
	if row.NetCents != h.netCents {
		t.Errorf("row netCents = %d, want the organiser's %d", row.NetCents, h.netCents)
	}
	if row.UnitPriceCents != 24_000 {
		t.Errorf("row unitPriceCents = %d, want the face value 24000", row.UnitPriceCents)
	}
}

// The CSV, which is the surface that matters most: it opens in a spreadsheet,
// where the difference between two columns is one subtraction away.
func TestAttendeeExportHidesThePlatformCommission(t *testing.T) {
	h := newHarness(t)

	recorder := h.get(t, "/api/v1/events/"+h.eventID+"/attendees.csv", "user")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET attendees.csv = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()

	for _, heading := range []string{"Taxa de serviço", "Total pago", "Taxa", "Bruto"} {
		if strings.Contains(body, heading) {
			t.Errorf("the export has a %q column; our commission must not be in a file the organiser opens", heading)
		}
	}

	// The organiser's own columns must still be there, or this is hiding the
	// feature rather than the fee.
	for _, heading := range []string{"Valor unitário", "Valor do organizador"} {
		if !strings.Contains(body, heading) {
			t.Errorf("the export lost its %q column", heading)
		}
	}

	// And the fee must not appear as a VALUE either. R$ 48,00 is the fee on
	// this fixture; the organiser's 480,00 is fine and must survive.
	feeAsReais := formatCents(h.feeCents)
	if strings.Contains(body, ";"+feeAsReais+";") {
		t.Errorf("the export contains %s, which is our commission on this order", feeAsReais)
	}
}

// formatCents renders centavos the way the CSV does, for the value assertion
// above: comma as the decimal separator, because that file is read in pt-BR.
func formatCents(cents int64) string {
	return fmt.Sprintf("%d,%02d", cents/100, cents%100)
}

// The assertions above are only worth anything if the fee is genuinely there
// to be leaked.
//
// Every "the commission is absent" test in this file looks for something NOT
// being present, and a test like that passes just as happily when the fixture
// never had a fee in the first place. This one reads the seeded order straight
// out of Postgres and insists the commission is on it — so the absence proved
// elsewhere is a decision the report made, not an accident of the fixture.
func TestTheCommissionIsInTheDatabaseItIsJustNotReported(t *testing.T) {
	h := newHarness(t)

	var stored struct {
		SubtotalCents   int64
		ServiceFeeCents int64
		TotalCents      int64
	}
	if err := h.db.Raw(`
		SELECT subtotal_cents, service_fee_cents, total_cents
		FROM orders WHERE event_id = ?`, h.eventID,
	).Scan(&stored).Error; err != nil {
		t.Fatalf("read the seeded order: %v", err)
	}

	if stored.ServiceFeeCents != h.feeCents || stored.ServiceFeeCents == 0 {
		t.Fatalf("seeded service fee = %d, want %d; the absence tests in this file would be vacuous",
			stored.ServiceFeeCents, h.feeCents)
	}
	if stored.TotalCents != stored.SubtotalCents+stored.ServiceFeeCents {
		t.Fatalf("the fixture does not add up: %d != %d + %d",
			stored.TotalCents, stored.SubtotalCents, stored.ServiceFeeCents)
	}

	// The gross the buyer paid is strictly larger than what the report shows,
	// which is the whole point: the organiser sees their share, not the sale.
	if stored.TotalCents <= h.netCents {
		t.Fatalf("the seeded gross %d is not above the reported net %d", stored.TotalCents, h.netCents)
	}

	recorder := h.get(t, "/api/v1/events/"+h.eventID+"/report", "user")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET report = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()

	// Neither the commission nor the gross may appear as a VALUE either. A
	// field renamed to something innocuous would still put the number in the
	// payload, and the number is what matters.
	//
	// Compared structurally rather than as text: "4800" is a substring of
	// "48000", so a substring search over the body would report the fee inside
	// the organiser's own net and fail for the wrong reason.
	numbers := numbersIn(t, recorder.Body.Bytes())
	for label, cents := range map[string]int64{
		"the commission": stored.ServiceFeeCents,
		"the gross":      stored.TotalCents,
	} {
		if numbers[cents] {
			t.Errorf("the report carries %s (%d) as a value", label, cents)
		}
	}
	// And the organiser's own number must be in there, or the report is empty
	// and everything above passed for the wrong reason.
	if !numbers[h.netCents] {
		t.Errorf("the report does not carry the organiser's net %d; body: %s", h.netCents, body)
	}
}

// numbersIn is every numeric value anywhere in a JSON document.
//
// Recursive over objects and arrays, because the amounts that matter are
// nested inside totals, slices, days and rows, and a check that only looked at
// the top level would miss all of them.
func numbersIn(t *testing.T, payload []byte) map[int64]bool {
	t.Helper()
	var document any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	found := map[int64]bool{}
	var walk func(node any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		case float64:
			found[int64(value)] = true
		}
	}
	walk(document)
	return found
}
