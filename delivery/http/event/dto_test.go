package event

import (
	"testing"

	"vozkot/domain/pricing"
	ticketdomain "vozkot/domain/ticket"
)

// The event page is where a buyer sees the fee FIRST.
//
// Everything about the service fee that is tested elsewhere happens after the
// buyer has already decided to buy: the order, the charge, the receipt. This
// function produces the number on the tier card, before anything is reserved,
// and it is the number the buyer will check the checkout total against. A page
// that quoted the face value while the order charged face plus ten per cent
// would be the most damaging way this feature could be wrong, because the
// difference only appears once the buyer has committed.
//
// The claim being tested is narrow and exact: what this function quotes for ONE
// ticket, multiplied by a quantity, is what checkout will charge for that
// quantity. Both sides read the same pricing.Fee, so the test is that neither
// applies it differently.
func TestTierQuotesTheSameFeeCheckoutWillCharge(t *testing.T) {
	fee, err := pricing.PlatformFee()
	if err != nil {
		t.Fatalf("PlatformFee(): %v", err)
	}

	tiers := []ticketdomain.Ticket{
		{ID: "tkt_round", PriceCents: 24_000, Currency: "BRL", Quantity: 10, Status: ticketdomain.StatusOnSale},
		// Ten per cent is 333.5 centavos: the case where per-ticket and
		// per-line rounding disagree.
		{ID: "tkt_odd", PriceCents: 3_335, Currency: "BRL", Quantity: 10, Status: ticketdomain.StatusOnSale},
		// A free tier must stay free on the page as well as on the order.
		{ID: "tkt_free", PriceCents: 0, Currency: "BRL", Quantity: 10, Status: ticketdomain.StatusOnSale},
	}

	responses := toTierResponses(tiers, fee)
	if len(responses) != len(tiers) {
		t.Fatalf("quoted %d tiers, want %d", len(responses), len(tiers))
	}

	for index, response := range responses {
		tier := tiers[index]

		// The face value is the organiser's and must be quoted untouched, so a
		// buyer comparing the page against the organiser's own advertising sees
		// the same price.
		if response.PriceCents != tier.PriceCents {
			t.Errorf("%s: quoted face value %d, want %d", tier.ID, response.PriceCents, tier.PriceCents)
		}

		wantFee := fee.On(tier.PriceCents)
		if response.FeeCents != wantFee {
			t.Errorf("%s: quoted fee %d, want %d", tier.ID, response.FeeCents, wantFee)
		}
		if response.TotalCents != tier.PriceCents+wantFee {
			t.Errorf("%s: quoted total %d, want %d", tier.ID, response.TotalCents, tier.PriceCents+wantFee)
		}

		// The page's own arithmetic has to hold, because this is the breakdown
		// the tier card renders line by line.
		if response.TotalCents != response.PriceCents+response.FeeCents {
			t.Errorf("%s: the quote does not add up: %d != %d + %d",
				tier.ID, response.TotalCents, response.PriceCents, response.FeeCents)
		}
	}

	// Free stays free. Ten per cent of nothing is nothing, and a
	// free-admission event that acquired a service fee would be the most
	// visible possible failure.
	free := responses[2]
	if free.FeeCents != 0 || free.TotalCents != 0 {
		t.Errorf("a free tier was quoted a fee: %+v", free)
	}
}

// Without a fee the page must quote the tier price and nothing else.
//
// The mirror of the test above: it computes its expectations from the same fee,
// so a bug that added a charge from somewhere other than pricing.Fee would
// survive it. This one hard-codes the answer.
func TestTierQuotesNoFeeWhenThereIsNone(t *testing.T) {
	tiers := []ticketdomain.Ticket{
		{ID: "tkt_1", PriceCents: 24_000, Currency: "BRL", Quantity: 10, Status: ticketdomain.StatusOnSale},
	}

	responses := toTierResponses(tiers, pricing.Fee{})
	if len(responses) != 1 {
		t.Fatalf("quoted %d tiers, want 1", len(responses))
	}
	quote := responses[0]

	if quote.FeeCents != 0 {
		t.Errorf("fee = %d with no fee configured, want 0", quote.FeeCents)
	}
	if quote.TotalCents != 24_000 {
		t.Errorf("total = %d, want the tier price 24000", quote.TotalCents)
	}
}
