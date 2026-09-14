package notification

import "context"

// Renderer turns a template plus its data into the body ONE channel carries.
//
// The channel is a parameter rather than a separate interface per medium
// because the choice is data, not a type: the container registers what it has,
// and a use case that asks for TemplateOrderConfirmed on a channel nobody
// renders gets a clean ErrUndeliverable instead of a nil dereference.
//
// Implementations must be safe for concurrent use and must not touch the disk
// or the network per call. A rendered receipt is on the critical path of every
// paid order, and at a hundred thousand of them an hour, re-reading a template
// file per send is the difference between a worker that is I/O bound on Resend
// and one that is I/O bound on itself.
type Renderer interface {
	Render(channel Channel, template Template, data map[string]any) (string, error)
}

// Message is one rendered notification, addressed and ready to go out.
type Message struct {
	// To is the channel-specific destination: an email address today, a phone
	// number when a messaging channel arrives.
	To string
	// Name is the recipient's display name, for adapters that can use one.
	Name string
	// Subject is meaningful to email and ignored elsewhere.
	Subject string
	// Body is whatever the channel carries, HTML for email.
	Body string
	// Category is the template this message came from, carried so an adapter
	// can label it at the provider. It is what lets an operator read the
	// deliverability of receipts separately from that of payment reminders
	// instead of as one undifferentiated stream.
	Category string
	// IdempotencyKey is passed to providers that honour one, so a delivery the
	// queue retries after the provider already accepted it does not arrive
	// twice. It is the same key the job was deduplicated on, which is what
	// makes the two layers agree on what "the same message" means.
	IdempotencyKey string
}

// Sender delivers messages over exactly one channel.
//
// A Sender owns its provider's manners: rate limits, retry-after, transport
// timeouts, and nothing else. It does not decide whether a failure is worth
// retrying beyond its own call: that is the queue's job, and the queue is the
// only thing that survives the process.
type Sender interface {
	Channel() Channel
	Send(ctx context.Context, message Message) error
}
