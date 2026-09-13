package notification

// Payload is a Request on the wire: what the job row holds and what the worker
// decodes an hour later if the provider was down that long.
//
// It is a separate type from Request on purpose. Request is the domain's
// shape, free of tags; Payload is the queue's, and the mapping between them is
// the same explicit seam the HTTP DTOs use. Keeping them apart means a field
// renamed in the domain cannot silently invalidate every job already sitting
// in the table.
type Payload struct {
	Channel  Channel  `json:"channel"`
	Template Template `json:"template"`
	Subject  string   `json:"subject,omitempty"`

	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`

	Data map[string]any `json:"data,omitempty"`
	// Key is the request's dedupe key, carried so the provider can be handed
	// the same idempotency key the queue deduplicated on.
	Key string `json:"key,omitempty"`
}

// NewPayload maps a request onto the wire.
func NewPayload(request Request) Payload {
	return Payload{
		Channel:  request.Channel,
		Template: request.Template,
		Subject:  request.Subject,
		Name:     request.Recipient.Name,
		Email:    request.Recipient.Email,
		Phone:    request.Recipient.Phone,
		Data:     request.Data,
		Key:      request.DedupeKey,
	}
}

// Request maps the wire back onto the domain.
func (p Payload) Request() Request {
	return Request{
		Channel:   p.Channel,
		Template:  p.Template,
		Recipient: Recipient{Name: p.Name, Email: p.Email, Phone: p.Phone},
		Subject:   p.Subject,
		Data:      p.Data,
		DedupeKey: p.Key,
	}
}
