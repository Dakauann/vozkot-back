package report

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	domain "vozkot/domain/report"
	"vozkot/infra/testsupport"
)

// The age bands exist twice: once in Go, for the CSV and anything that reads a
// single row, and once in SQL, because bucketing a whole event in memory would
// mean reading every order of it. Duplication of a rule is a real cost and this
// test is what pays for it — every age is walked through BOTH, so a boundary
// edited in one place and not the other fails here rather than quietly putting
// the same person in two different bands depending on which screen asked.
func TestSQLBucketsMatchTheDomain(t *testing.T) {
	db := testsupport.Database(t)
	ctx := context.Background()

	for age := -1; age <= 120; age++ {
		var fromSQL string
		// The same CASE the repository groups by, evaluated against one age.
		query := "SELECT " + strings.ReplaceAll(ageBucketSQL, "o.buyer_age_years", fmt.Sprintf("%d", age))
		if err := db.WithContext(ctx).Raw(query).Scan(&fromSQL).Error; err != nil {
			t.Fatalf("age %d: %v", age, err)
		}
		if want := string(domain.BracketFor(age)); fromSQL != want {
			t.Errorf("age %d: SQL says %q, Go says %q", age, fromSQL, want)
		}
	}
}

// A masked document has to be unusable and still recognisable. Both halves
// matter: too much and it is a CPF in a spreadsheet an organiser emails around,
// too little and nobody can match it to the card somebody shows at the door.
func TestMaskDocumentKeepsEnoughAndNoMore(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"123":         "***",
		"12345678909": "123***09",
		"AB123456":    "AB1***56",
	}
	for document, want := range cases {
		if got := MaskDocument(document); got != want {
			t.Errorf("MaskDocument(%q) = %q, want %q", document, got, want)
		}
	}
	// The whole number must never survive the mask.
	const cpf = "12345678909"
	if masked := MaskDocument(cpf); masked == cpf {
		t.Fatal("the document came back unmasked")
	}
}

// An event nobody bought must answer with zeroes and empty breakdowns, not an
// error and not nil slices: the dashboard renders it as "nenhuma venda ainda",
// which is a real state every new event is in.
func TestSalesForAnEventWithNoOrders(t *testing.T) {
	db := testsupport.Database(t)
	repository := NewReportRepository(db)

	sales, err := repository.Sales(context.Background(), fmt.Sprintf("evt_empty_%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("Sales(): %v", err)
	}
	if sales.Totals.Orders != 0 || sales.Totals.Tickets != 0 || sales.Totals.NetCents != 0 {
		t.Fatalf("an event with no orders reported sales: %+v", sales.Totals)
	}
	for name, slices := range map[string][]domain.Slice{
		"gender": sales.ByGender,
		"age":    sales.ByAge,
		"uf":     sales.ByUF,
		"city":   sales.ByCity,
		"tier":   sales.ByTier,
	} {
		if len(slices) != 0 {
			t.Errorf("%s breakdown returned %d rows for an event with no orders", name, len(slices))
		}
	}
	if len(sales.ByDay) != 0 {
		t.Errorf("day breakdown returned %d rows for an event with no orders", len(sales.ByDay))
	}
}

// Grouping by an expression nobody registered must produce nothing rather than
// a query. It is the guard that keeps the interpolated column name safe if a
// future caller ever wires a request parameter through.
func TestGroupByRefusesAnUnregisteredExpression(t *testing.T) {
	db := testsupport.Database(t)
	repository := NewReportRepository(db)

	_, err := repository.groupBy(context.Background(), "evt_1", "o.buyer_name; DROP TABLE orders", 0)
	if err == nil {
		t.Fatal("an unregistered grouping expression was accepted")
	}
}
