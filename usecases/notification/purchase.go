package notification

import (
	"context"
	"strconv"
	"strings"

	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/notification"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	"vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
)

// Purchases turns what happened to an order into what the buyer is told.
//
// It is the only place that knows both halves, an order's state machine and a
// message's shape, and it exists so neither of the two use cases either side
// of it has to. usecases/payment stays about money and stock; the templates
// stay about words; the mapping between them lives here, once, in the language
// the buyer reads.
//
// Like Notifier, a nil *Purchases is the "notifications are off" value and
// every method is safe on it.
type Purchases struct {
	notifier *Notifier
	// siteURL is where a buyer goes to see the order. Emails that show a total
	// and no way to act on it are the ones support has to answer.
	siteURL string
}

func NewPurchases(notifier *Notifier, siteURL string) *Purchases {
	return &Purchases{notifier: notifier, siteURL: strings.TrimRight(strings.TrimSpace(siteURL), "/")}
}

// ChargeIssued tells the buyer how to pay, and is sent once the provider has
// answered with something actionable, a PIX code, a boleto, not when the
// order was opened. At checkout there is nothing to send: the charge is still
// a job in a queue, and an email saying "pay now" with no code to pay against
// is worse than no email.
//
// jobs is the queue handle bound to the caller's transaction; the returned job
// is announced to the broker after that transaction commits.
func (p *Purchases) ChargeIssued(ctx context.Context, jobs queue.Queue, item *orderdomain.Order, tier *ticketdomain.Ticket, happening *eventdomain.Event) (*queue.Job, error) {
	if p == nil || item == nil {
		return nil, nil
	}
	data := p.orderData(item, tier, happening)
	data["PaymentDeadline"] = longDate(item.HoldExpiresAt)
	data["PixCopyPaste"] = item.PixCopyPaste
	data["HasPix"] = item.PixCopyPaste != ""

	return p.notifier.Enqueue(ctx, jobs, domain.Request{
		Channel:   domain.ChannelEmail,
		Template:  domain.TemplateOrderPending,
		Recipient: recipient(item),
		Subject:   "Falta pouco: conclua o pagamento do pedido " + reference(item.ID),
		Data:      data,
		// Once per order, for the life of the order. A charge is created once
		// and reconciled many times; only the first crossing sends.
		DedupeKey: dedupeKey(domain.TemplateOrderPending, item.ID),
	})
}

// OrderPaid is the receipt: the money arrived and the ingressos are the
// buyer's. It is raised from inside the settlement transaction, so it commits
// with the sale or not at all.
func (p *Purchases) OrderPaid(ctx context.Context, jobs queue.Queue, item *orderdomain.Order, tier *ticketdomain.Ticket, happening *eventdomain.Event) (*queue.Job, error) {
	if p == nil || item == nil {
		return nil, nil
	}
	data := p.orderData(item, tier, happening)
	data["PaidAt"] = ""
	if item.PaidAt != nil {
		data["PaidAt"] = shortDateTime(*item.PaidAt)
	}

	return p.notifier.Enqueue(ctx, jobs, domain.Request{
		Channel:   domain.ChannelEmail,
		Template:  domain.TemplateOrderConfirmed,
		Recipient: recipient(item),
		Subject:   "Pagamento confirmado: pedido " + reference(item.ID),
		Data:      data,
		DedupeKey: dedupeKey(domain.TemplateOrderConfirmed, item.ID),
	})
}

// orderData is the snapshot both messages are rendered from: everything the
// buyer needs to recognise the purchase without opening anything else.
//
// Built here rather than in the template because the template's job is layout.
// Money is formatted once, by the code that knows it is int64 centavos; a
// template that divides by a hundred is a template that eventually rounds
// somebody's total.
func (p *Purchases) orderData(item *orderdomain.Order, tier *ticketdomain.Ticket, happening *eventdomain.Event) map[string]any {
	// Every key is set, always, even when there is nothing to put in it.
	//
	// The data reaches the renderer as a map decoded from JSON, and a Go
	// template asked for a key a map does not have prints "<no value>", into
	// a buyer's receipt, next to their money. An empty string renders as
	// nothing and is skipped by the components that check their arguments, so
	// a tier that vanished costs a row rather than a support ticket.
	data := map[string]any{
		"OrderID":        item.ID,
		"OrderReference": reference(item.ID),
		"BuyerName":      firstName(item.BuyerName),
		"BuyerFullName":  item.BuyerName,
		"BuyerEmail":     item.BuyerEmail,
		"Quantity":       item.TotalQuantity(),
		"UnitPrice":      unitPrice(item),
		"Total":          money(item.TotalCents, item.Currency),
		// The lines, so a receipt for two Pista and one Camarote says so
		// instead of flattening into "3 ingressos". Pre-formatted here for the
		// same reason the totals are: a template that formats money is a
		// template that eventually rounds it.
		"Items":         lineItems(item),
		"PaymentMethod": methodLabel(item.PaymentMethod),
		"OrderURL":      p.orderURL(item.ID),

		"EventName":   "",
		"TicketTitle": "",
		"ItemSummary": summary(item),
		"Venue":       "",
		"City":        "",
		"StartsAt":    "",
		"Place":       "",
	}
	// The tier names the seat; the event names the night. Both are read
	// defensively, because a receipt that is missing a line is better than a
	// receipt that never went out.
	if tier != nil {
		data["TicketTitle"] = tier.Title
	}
	if happening != nil {
		data["EventName"] = happening.Name
		data["Venue"] = happening.Location.Venue
		data["City"] = happening.Location.City
		data["StartsAt"] = longDate(happening.StartsAt)
		data["Place"] = place(happening)
	}
	return data
}

func (p *Purchases) orderURL(orderID string) string {
	if p.siteURL == "" {
		return ""
	}
	return p.siteURL + "/pedidos/" + orderID
}

func recipient(item *orderdomain.Order) domain.Recipient {
	return domain.Recipient{Name: item.BuyerName, Email: item.BuyerEmail}
}

// dedupeKey is the queue's key and the provider's idempotency key at once, so
// "the same message" means the same thing in the job table and at Resend.
func dedupeKey(template domain.Template, orderID string) string {
	return queue.TypeSendNotification + ":" + string(template) + ":" + orderID
}

// reference is the order id a buyer reads out to support: short, uppercase and
// without the internal prefix, but still enough of the id to find the row.
func reference(orderID string) string {
	trimmed := strings.TrimPrefix(orderID, "ord_")
	if len(trimmed) > 8 {
		trimmed = trimmed[:8]
	}
	return strings.ToUpper(trimmed)
}

// firstName is what a greeting uses. "Olá, Maria" reads as a person writing;
// "Olá, Maria Aparecida Dos Santos" reads as a database.
func firstName(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func place(happening *eventdomain.Event) string {
	if happening.Location.City == "" {
		return happening.Location.Venue
	}
	return happening.Location.Venue + " · " + happening.Location.City
}

func methodLabel(method paymentdomain.Method) string {
	switch method {
	case paymentdomain.MethodPix:
		return "PIX"
	case paymentdomain.MethodBoleto:
		return "Boleto"
	case paymentdomain.MethodCard:
		return "Cartão"
	default:
		return string(method)
	}
}

// unitPrice is the per-ticket price, when there is one to state.
//
// A single-tier order has one; an order spanning Pista at R$ 90 and Camarote at
// R$ 240 does not, and inventing an average would print a number the buyer was
// never charged. Empty is the honest answer, and the receipt shows the lines
// instead.
func unitPrice(item *orderdomain.Order) string {
	if len(item.Items) != 1 {
		return ""
	}
	return money(item.Items[0].UnitPriceCents, item.Currency)
}

// lineItems renders each line for the receipt's table.
func lineItems(item *orderdomain.Order) []map[string]any {
	lines := make([]map[string]any, 0, len(item.Items))
	for _, line := range item.Items {
		lines = append(lines, map[string]any{
			"Title":     line.TicketTitle,
			"Quantity":  line.Quantity,
			"UnitPrice": money(line.UnitPriceCents, item.Currency),
			"Total":     money(line.TotalCents, item.Currency),
		})
	}
	return lines
}

// summary is the one-line version, for a subject or a preview: "2x Pista, 1x
// Camarote".
func summary(item *orderdomain.Order) string {
	if len(item.Items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(item.Items))
	for _, line := range item.Items {
		title := line.TicketTitle
		if title == "" {
			continue
		}
		parts = append(parts, strconv.Itoa(line.Quantity)+"x "+title)
	}
	return strings.Join(parts, ", ")
}
