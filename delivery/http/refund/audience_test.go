package refund

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/refund"
	userdomain "vozkot/domain/user"
	usecase "vozkot/usecases/refund"
)

// Who sees our commission on a refund.
//
// A refund returns the ticket price AND the service fee to the buyer, so the
// figure has two halves with two different owners. The buyer must be told the
// whole number: the Terms promise the fee comes back and naming it is what
// prevents the dispute. The organiser must be told only their half: the face
// value is what leaves their revenue, and our commission returning to the
// buyer is between us and the buyer.
//
// Three callers reach the same endpoints, so this is tested through the
// ENCODED JSON rather than the struct: an omitted pointer and a zeroed int are
// the same field to Go and completely different answers to a client.

const (
	grossCents     = 11_000
	feeCents       = 1_000
	organiserCents = grossCents - feeCents
)

func sample() *domain.Request {
	return &domain.Request{
		ID:          "rfr_1",
		OrderID:     "ord_1",
		EventID:     "evt_1",
		BuyerID:     "usr_buyer",
		RequestedBy: "usr_buyer",
		Reason:      domain.ReasonBuyerWithdrawal,
		Status:      domain.StatusPending,
		AmountCents: grossCents,
		FeeCents:    feeCents,
	}
}

// encoded is the response as a client actually receives it.
func encoded(t *testing.T, value any) (string, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return string(raw), decoded
}

func TestSeesPlatformShare(t *testing.T) {
	actorOf := func(id string, role userdomain.Role) authdomain.Actor {
		return authdomain.Actor{ID: id, Role: role}
	}
	cases := []struct {
		name   string
		actor  authdomain.Actor
		owners []string
		want   bool
	}{
		{"the buyer sees their own money", actorOf("usr_buyer", userdomain.RoleUser), []string{"usr_buyer"}, true},
		{"the person who filed it sees it", actorOf("usr_agent", userdomain.RoleUser), []string{"usr_buyer", "usr_agent"}, true},
		{"an operator sees everything", actorOf("usr_ops", userdomain.RoleAdmin), []string{"usr_buyer"}, true},
		{"the organiser does not", actorOf("usr_owner", userdomain.RoleUser), []string{"usr_buyer"}, false},
		{"a stranger does not", actorOf("usr_other", userdomain.RoleUser), []string{"usr_buyer"}, false},
		// A door sale has no account on it. An empty owner must not match an
		// unauthenticated caller into "this is yours".
		{"an empty owner matches nobody", actorOf("", userdomain.RoleUser), []string{""}, false},
		{"an empty caller matches nothing", actorOf("", userdomain.RoleUser), []string{"usr_buyer"}, false},
		// The zero Actor is what a missing session produces.
		{"the zero actor owns nothing", authdomain.Actor{}, []string{"usr_buyer"}, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := usecase.SeesPlatformShare(testCase.actor, testCase.owners...)
			if got != testCase.want {
				t.Fatalf("SeesPlatformShare(%+v, %v) = %v, want %v",
					testCase.actor, testCase.owners, got, testCase.want)
			}
		})
	}
}

// The buyer's view: the whole figure, because it is their money.
func TestRefundResponseShowsTheBuyerEverything(t *testing.T) {
	raw, fields := encoded(t, toResponse(sample(), true))

	amount, ok := fields["amountCents"]
	if !ok {
		t.Fatal("the buyer was not told what comes back")
	}
	if int64(amount.(float64)) != grossCents {
		t.Errorf("amountCents = %v, want %d", amount, grossCents)
	}
	fee, ok := fields["feeCents"]
	if !ok {
		t.Fatal("the buyer was not told the service fee is included; the Terms promise it is")
	}
	if int64(fee.(float64)) != feeCents {
		t.Errorf("feeCents = %v, want %d", fee, feeCents)
	}
	if int64(fields["organiserCents"].(float64)) != organiserCents {
		t.Errorf("organiserCents = %v, want %d", fields["organiserCents"], organiserCents)
	}
	// The three have to be consistent, because a screen will show two of them
	// and a person will subtract.
	if int64(amount.(float64)) != int64(fee.(float64))+int64(fields["organiserCents"].(float64)) {
		t.Errorf("the response does not add up: %s", raw)
	}
}

// The organiser's view: their half, and no way to reach ours.
func TestRefundResponseHidesOurShareFromTheOrganiser(t *testing.T) {
	raw, fields := encoded(t, toResponse(sample(), false))

	for _, key := range []string{"amountCents", "feeCents"} {
		if _, present := fields[key]; present {
			t.Errorf("the organiser was sent %q: %s", key, raw)
		}
	}
	if !strings.Contains(raw, "organiserCents") {
		t.Fatalf("the organiser was not told their own share: %s", raw)
	}
	if int64(fields["organiserCents"].(float64)) != organiserCents {
		t.Errorf("organiserCents = %v, want %d", fields["organiserCents"], organiserCents)
	}

	// Omitted, not zeroed. A zero would render as "R$ 0,00 comes back", which
	// is a wrong answer rather than a withheld one.
	if strings.Contains(raw, `"amountCents":0`) || strings.Contains(raw, `"feeCents":0`) {
		t.Errorf("our side was zeroed instead of omitted: %s", raw)
	}

	// And the numbers must not be in the payload under any other name.
	numbers := numbersIn(t, []byte(raw))
	if numbers[grossCents] {
		t.Errorf("the gross %d is in the organiser's payload: %s", grossCents, raw)
	}
	if numbers[feeCents] {
		t.Errorf("our commission %d is in the organiser's payload: %s", feeCents, raw)
	}
	if !numbers[organiserCents] {
		t.Errorf("the organiser's own share %d is missing: %s", organiserCents, raw)
	}
}

// Eligibility is the other place the figure appears, the dialog that asks
// "cancel this order?", and it has to follow the same rule.
func TestEligibilityFollowsTheSameRule(t *testing.T) {
	base := usecase.Eligibility{
		AmountCents:    grossCents,
		FeeCents:       feeCents,
		OrganiserCents: organiserCents,
		RefundsFees:    true,
	}

	t.Run("buyer", func(t *testing.T) {
		shown := base
		shown.ShowsPlatformShare = true
		_, fields := encoded(t, toEligibility(shown, nil))
		if _, present := fields["amountCents"]; !present {
			t.Error("the buyer was not told what comes back")
		}
		if _, present := fields["feeCents"]; !present {
			t.Error("the buyer was not told the fee is included")
		}
	})

	t.Run("organiser", func(t *testing.T) {
		hidden := base
		hidden.ShowsPlatformShare = false
		raw, fields := encoded(t, toEligibility(hidden, nil))
		for _, key := range []string{"amountCents", "feeCents"} {
			if _, present := fields[key]; present {
				t.Errorf("the organiser was sent %q: %s", key, raw)
			}
		}
		if int64(fields["organiserCents"].(float64)) != organiserCents {
			t.Errorf("organiserCents = %v, want %d", fields["organiserCents"], organiserCents)
		}
	})

	// The open request nested inside an eligibility answer is a second route to
	// the same numbers, and it must not be a way around the rule.
	t.Run("the nested open request follows too", func(t *testing.T) {
		hidden := base
		hidden.ShowsPlatformShare = false
		raw, _ := encoded(t, toEligibility(hidden, sample()))
		numbers := numbersIn(t, []byte(raw))
		if numbers[grossCents] || numbers[feeCents] {
			t.Errorf("the nested request leaked our side: %s", raw)
		}
	})
}

// numbersIn is every numeric value anywhere in a JSON document, so a field
// renamed to something innocuous still cannot smuggle an amount out.
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

// The wiring, not just the mapper.
//
// Every test above calls toResponse directly with a boolean, so all of them
// would still pass if writeRequest computed that boolean wrongly: passing the
// organiser's id where the buyer's belongs, or defaulting to true. This drives
// the helper the four endpoints actually use, with the claims the middleware
// actually puts on the request.
func TestWriteRequestDecidesFromTheCallersClaims(t *testing.T) {
	cases := []struct {
		name   string
		claims *authdomain.Claims
		shown  bool
	}{
		{"the buyer", &authdomain.Claims{UserID: "usr_buyer", Role: "user"}, true},
		{"an operator", &authdomain.Claims{UserID: "usr_ops", Role: "admin"}, true},
		{"the organiser", &authdomain.Claims{UserID: "usr_owner", Role: "user"}, false},
		// No session at all should never be the permissive branch.
		{"nobody", nil, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeRequest(recorder, http.StatusOK, sample(), testCase.claims)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			var envelope struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode: %v", err)
			}

			_, hasAmount := envelope.Data["amountCents"]
			_, hasFee := envelope.Data["feeCents"]
			if hasAmount != testCase.shown || hasFee != testCase.shown {
				t.Errorf("amountCents present=%v feeCents present=%v, want %v: %s",
					hasAmount, hasFee, testCase.shown, recorder.Body.String())
			}
			// Their own share is always there, whoever asked.
			if int64(envelope.Data["organiserCents"].(float64)) != organiserCents {
				t.Errorf("organiserCents = %v, want %d",
					envelope.Data["organiserCents"], organiserCents)
			}
		})
	}
}
