package notification

import (
	"context"
	"errors"
	"fmt"

	domain "vozkot/domain/notification"
	"vozkot/domain/queue"
)

// Service delivers one notification: it renders the template for the channel
// and hands the result to that channel's adapter.
//
// It is the worker's handler and nothing else. There is no loop, no backoff
// and no attempt counter in here, because the job row already has all three
// and they survive a deploy, which is the one property an in-process retry
// loop can never have.
type Service struct {
	renderer domain.Renderer
	senders  map[domain.Channel]domain.Sender
}

func NewService(renderer domain.Renderer, senders ...domain.Sender) *Service {
	registered := make(map[domain.Channel]domain.Sender, len(senders))
	for _, sender := range senders {
		if sender == nil {
			continue
		}
		registered[sender.Channel()] = sender
	}
	return &Service{renderer: renderer, senders: registered}
}

// Channels lists what this service can actually deliver, so the container can
// tell the Notifier the same thing and the two never disagree.
func (s *Service) Channels() []domain.Channel {
	channels := make([]domain.Channel, 0, len(s.senders))
	for channel := range s.senders {
		channels = append(channels, channel)
	}
	return channels
}

// Deliver renders and sends one notification.
//
// The error it returns is a verdict for the queue, and the split matters:
//
//   - queue.Permanent parks the job on the first attempt. Reserved for what no
//     retry can fix: a channel with no sender, a recipient with no address, a
//     template that will not render. Trying those twenty times over an hour
//     tells nobody anything the first attempt did not, and hides work that
//     needs a person inside the retry queue.
//   - a plain error is retried with backoff. Every provider failure lands here,
//     including the ones that look like the caller's fault: Resend collapses a
//     rejected address and a 503 into the same untyped error, and parking a
//     receipt because the provider had a bad thirty seconds is the more
//     expensive mistake by far.
func (s *Service) Deliver(ctx context.Context, payload domain.Payload) error {
	request := payload.Request()
	if err := request.Validate(); err != nil {
		return queue.Permanent(err)
	}

	sender, ok := s.senders[request.Channel]
	if !ok {
		return queue.Permanent(fmt.Errorf("%w: %w: %s",
			domain.ErrUndeliverable, domain.ErrNoSender, request.Channel))
	}

	body, err := s.renderer.Render(request.Channel, request.Template, request.Data)
	if err != nil {
		// Templates are parsed at boot, so reaching here means the DATA is
		// wrong for this template. Another attempt renders the same data
		// against the same template and fails the same way.
		return queue.Permanent(fmt.Errorf("%w: render %s for %s: %w",
			domain.ErrUndeliverable, request.Template, request.Channel, err))
	}

	message := domain.Message{
		To:       request.Recipient.Address(request.Channel),
		Name:     request.Recipient.Name,
		Subject:  request.Subject,
		Body:     body,
		Category: string(request.Template),
		// The provider is handed the same key the queue deduplicated on. A job
		// the provider accepted but that failed to record itself, a worker
		// killed between the send and the row update; is reclaimed by the
		// stale sweep and sent again; the key is what makes that second send
		// arrive zero times instead of twice.
		IdempotencyKey: request.DedupeKey,
	}
	if err := sender.Send(ctx, message); err != nil {
		if errors.Is(err, domain.ErrUndeliverable) {
			return queue.Permanent(err)
		}
		return err
	}
	return nil
}
