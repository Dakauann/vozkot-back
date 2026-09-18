// Package notification is what this system says to a buyer, written once per
// MESSAGE rather than once per medium.
//
// Today every message leaves as email through Resend. The next one leaves over
// WhatsApp, and when it does the only thing that changes is which Sender the
// container registers for that channel, not checkout, not settlement, not the
// queue, not one call site. That is the whole reason this package exists: a
// Request names WHO to tell, WHAT happened and on WHICH channel, and nothing
// above infra ever names a provider or a body format.
//
// The delivery guarantees are deliberately NOT here. A notification is durable
// work like any other, so it rides domain/queue: the job row is written inside
// the transaction that made the fact true, which is what stops a receipt going
// out for a payment that rolled back and stops one being lost for a payment
// that committed.
package notification

import (
	"errors"
	"strings"
)

// Channel is the medium a message leaves by.
//
// A channel is not a provider. Resend is how email happens today and could be
// swapped tomorrow without a caller noticing; "email" is what the buyer
// receives, and that is the part a use case gets to choose.
type Channel string

const (
	ChannelEmail Channel = "email"
	// ChannelWhatsApp and ChannelSMS both reach a phone, and they are separate
	// channels rather than one "phone" channel because the buyer can tell them
	// apart: a screen that says "check your WhatsApp" is wrong if the code
	// arrived by text. A use case that does not care picks whichever the
	// container registered first.
	//
	// Both are carried by the same Sender today. That is an infra detail and
	// not a reason to merge them here.
	ChannelWhatsApp Channel = "whatsapp"
	ChannelSMS      Channel = "sms"
)

func (c Channel) Valid() bool {
	switch c {
	case ChannelEmail, ChannelWhatsApp, ChannelSMS:
		return true
	default:
		return false
	}
}

// Phone reports whether a channel delivers to a phone number rather than an
// inbox. Used where the distinction is genuinely about the ADDRESS, which
// field of a Recipient applies, which renderer produces the body, and nowhere
// else: anything that needs to know more than that wants a Channel.
func (c Channel) Phone() bool {
	switch c {
	case ChannelWhatsApp, ChannelSMS:
		return true
	default:
		return false
	}
}

// Template names WHAT is being said, never how it looks. The same
// TemplateOrderConfirmed is an HTML receipt on email and would be a short text
// on WhatsApp; which one gets rendered is the Renderer's business.
type Template string

const (
	// TemplateOrderPending is "here is how to pay", sent once the provider has
	// issued a charge and the buyer finally has something to act on. Sending it
	// at checkout instead would promise a PIX code that does not exist yet.
	TemplateOrderPending Template = "order_pending"
	// TemplateOrderConfirmed is the receipt: the money arrived and the
	// ingressos belong to the buyer.
	TemplateOrderConfirmed Template = "order_confirmed"
	// TemplateSignInCode carries a one-time code. It is the only message in
	// the system whose CONTENTS are a live credential, which is why it is
	// never deduplicated by content and never retained: the job that sends it
	// is keyed on the challenge, and the challenge is swept once it lapses.
	TemplateSignInCode Template = "sign_in_code"
)

func (t Template) Valid() bool {
	switch t {
	case TemplateOrderPending, TemplateOrderConfirmed, TemplateSignInCode:
		return true
	default:
		return false
	}
}

// Recipient is one person, addressable on any channel this system grows into.
//
// No json tags. This is the domain's own shape; the queue's wire format is
// Payload's business and the two are mapped explicitly, exactly as the HTTP
// DTOs map the other entities.
type Recipient struct {
	Name  string
	Email string
	// Phone is digits with a country code, as domain/user.NormalizePhone
	// produces. Provider-specific re-addressing, the ninth digit a Brazilian
	// mobile carries on WhatsApp, say, belongs to whoever is sending, not to
	// the recipient.
	Phone string
}

// Address is where a channel delivers to, or "" when this recipient cannot be
// reached on it.
func (r Recipient) Address(channel Channel) string {
	switch {
	case channel == ChannelEmail:
		return strings.ToLower(strings.TrimSpace(r.Email))
	case channel.Phone():
		return strings.TrimSpace(r.Phone)
	default:
		return ""
	}
}

// Request is one message somebody wants sent.
//
// Data is a SNAPSHOT, not a set of ids, and that is the one place this package
// departs from the queue's "carry ids, read current state" rule. A receipt
// states what was true when the buyer paid. Re-reading the order an hour later,
// after an operator renamed the event, or corrected a venue, would email a
// different receipt than the one the money was taken for.
type Request struct {
	Channel   Channel
	Template  Template
	Recipient Recipient
	// Subject is the email subject line and is ignored by channels that have
	// no such thing.
	Subject string
	Data    map[string]any
	// DedupeKey makes the send at-most-once for one logical event. It becomes
	// the job's dedupe key, so the database, not a cache, not a mutex,
	// decides whether a second "your payment arrived" is a duplicate.
	DedupeKey string
}

var (
	// ErrUndeliverable marks a failure no retry can fix: an unknown channel, a
	// recipient with no address on it, a template that does not render. The
	// use case turns these into parked jobs on the first attempt instead of
	// spending a whole retry budget rediscovering them.
	ErrUndeliverable = errors.New("notification cannot be delivered")

	ErrUnknownChannel  = errors.New("unknown notification channel")
	ErrUnknownTemplate = errors.New("unknown notification template")
	ErrNoRecipient     = errors.New("notification has no recipient on this channel")
	ErrNoSender        = errors.New("no sender is registered for this channel")
	ErrNotConfigured   = errors.New("notification provider is not configured")
)

// Validate reports whether this request can ever be delivered. Everything it
// rejects is wrapped in ErrUndeliverable, because none of it improves by
// waiting.
func (r Request) Validate() error {
	if !r.Channel.Valid() {
		return errors.Join(ErrUndeliverable, ErrUnknownChannel)
	}
	if !r.Template.Valid() {
		return errors.Join(ErrUndeliverable, ErrUnknownTemplate)
	}
	if r.Recipient.Address(r.Channel) == "" {
		return errors.Join(ErrUndeliverable, ErrNoRecipient)
	}
	return nil
}

// MaxDeliveryAttempts is how many times a notification job is tried before it
// is parked.
//
// Higher than the payment jobs' budget, and for the opposite reason. A charge
// that will not go through needs a human quickly; a receipt only needs the
// provider to come back. With the queue's five-minute backoff ceiling, twenty
// attempts ride out roughly an hour of Resend being down, long enough that a
// provider incident costs nobody their ticket confirmation, short enough that
// a genuinely bad address still lands in the dead list the same shift.
const MaxDeliveryAttempts = 20
