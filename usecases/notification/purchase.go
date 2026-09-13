package notification

import (
	"context"
	"strings"

	domain "vozkot/domain/notification"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	"vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
)

// Purchases turns what happened to an order into what the buyer is told.
//
// It is the only place that knows both halves — an order's state machine and a
// message's shape — and it exists so neither of the two use cases either side
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
// answered with something actionable — a PIX code, a boleto — not when the
// order was opened. At checkout there is nothing to send: the charge is still
// a job in a queue, and an email saying "pay now" with no code to pay against
// is worse than no email.
//
// jobs is the queue handle bound to the caller's transaction; the returned job
// is announced to the broker after that transaction commits.
func (p *Purchases) ChargeIssued(ctx context.Context, jobs queue.Queue, item *orderdomain.Order, tier *ticketdomain.Ticket) (*queue.Job, error) {
	if p == nil || item == nil {
		return nil, nil
	}
	data := p.orderData(item, tier)
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
func (p *Purchases) OrderPaid(ctx context.Context, jobs queue.Queue, item *orderdomain.Order, tier *ticketdomain.Ticket) (*queue.Job, error) {
	if p == nil || item == nil {
		return nil, nil
	}
	data := p.orderData(item, tier)
	data["PaidAt"] = ""
	if item.PaidAt != nil {
		data["PaidAt"] = shortDateTime(*item.PaidAt)
	}

	return p.notifier.Enqueue(ctx, jobs, domain.Request{
		Channel:   domain.ChannelEmail,
		Template:  domain.TemplateOrderConfirmed,
		Recipient: recipient(item),
		Subject:   "Pagamento confirmado — pedido " + reference(item.ID),
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
func (p *Purchases) orderData(item *orderdomain.Order, tier *ticketdomain.Ticket) map[string]any {
	// Every key is set, always, even when there is nothing to put in it.
	//
	// The data reaches the renderer as a map decoded from JSON, and a Go
	// template asked for a key a map does not have prints "<no value>" — into
	// a buyer's receipt, next to their money. An empty string renders as
	// nothing and is skipped by the components that check their arguments, so
	// a tier that vanished costs a row rather than a support ticket.
	data := map[string]any{
		"OrderID":        item.ID,
		"OrderReference": reference(item.ID),
		"BuyerName":      firstName(item.BuyerName),
		"BuyerFullName":  item.BuyerName,
		"BuyerEmail":     item.BuyerEmail,
		"Quantity":       item.Quantity,
		"UnitPrice":      money(item.UnitPriceCents, item.Currency),
		"Total":          money(item.TotalCents, item.Currency),
		"PaymentMethod":  methodLabel(item.PaymentMethod),
		"OrderURL":       p.orderURL(item.ID),

		"EventName":   "",
		"TicketTitle": "",
		"Venue":       "",
		"City":        "",
		"StartsAt":    "",
		"Place":       "",
	}
	if tier != nil {
		data["EventName"] = tier.EventName
		data["TicketTitle"] = tier.Title
		data["Venue"] = tier.Venue
		data["City"] = tier.City
		data["StartsAt"] = longDate(tier.StartsAt)
		data["Place"] = place(tier)
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

func place(tier *ticketdomain.Ticket) string {
	if tier.City == "" {
		return tier.Venue
	}
	return tier.Venue + " · " + tier.City
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
