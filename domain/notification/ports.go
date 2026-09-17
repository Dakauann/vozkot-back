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

// ChromeInliner is the optional half of Renderer: a renderer whose every body
// references an inline part of its own, the way the email layout's header
// points at the wordmark.
//
// Optional rather than part of Renderer because it is a property of one
// rendering of one channel, not of rendering. A body that references nothing,
// or a channel that cannot carry an attachment, implements nothing and the
// service asks it nothing.
type ChromeInliner interface {
	// Chrome returns the parts every body on this channel references. The
	// slice is not retained or modified by the caller.
	Chrome(channel Channel) []Inline
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
	// Inline are images the body references by content id.
	//
	// A ticket has to carry a QR into an inbox, and there are only two ways to
	// do that. A data: URI does not work — Gmail strips it out of an img src,
	// so the buyer sees a broken image where their ticket should be. A hosted
	// URL works but puts the code into Google's image proxy and its cache. So
	// the image travels WITH the message, as a multipart/related part the body
	// points at with cid:, which is what every airline boarding pass does.
	//
	// Adapters for channels that cannot carry an attachment ignore this; the
	// body is written so that dropping the image still leaves a usable message,
	// because the printed code is in the text beside it.
	Inline []Inline
}

// Inline is one image carried inside the message.
type Inline struct {
	// ContentID is what the body references: <img src="cid:THIS">.
	ContentID string
	Filename  string
	// ContentType is the media type, for example image/png.
	ContentType string
	Content     []byte
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
