package notification

import (
	"context"
	"strconv"
	"time"

	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/notification"
	queueUsecase "vozkot/usecases/queue"
)

// Delivering a one-time code.
//
// This is the only message the system sends whose CONTENTS are a live
// credential, and that shapes three decisions:
//
//   - It goes through the same durable queue as the money. A mail provider
//     having a bad minute must not mean a person cannot sign in; the job
//     retries.
//   - Its dedupe key is the CODE, not the address. Two codes requested for one
//     address are two different codes and both must be sent, a key on the
//     address would silently drop the second, leaving somebody holding a code
//     the server has already replaced.
//   - It is NEVER logged, and there is no path in which it is. With no provider
//     configured this refuses to issue a code at all rather than printing one:
//     a credential written to stdout is a credential in whatever aggregator
//     collects stdout, retained for as long as that keeps logs, readable by
//     everyone who can read them. "Only in development" is not a property of a
//     log line; it is a property of a config value somebody can set anywhere.

// CodeSender queues verification codes for delivery.
type CodeSender struct {
	notifier   *Notifier
	dispatcher *queueUsecase.Dispatcher
	// channel says which of the two this instance carries, so the error it
	// returns when unconfigured names the right thing.
	channel domain.Channel
}

func NewCodeSender(notifier *Notifier, dispatcher *queueUsecase.Dispatcher) *CodeSender {
	return &CodeSender{notifier: notifier, dispatcher: dispatcher, channel: domain.ChannelEmail}
}

// NewPhoneCodeSender carries codes to a phone instead of an inbox.
//
// The channel is a parameter rather than a constant because this use case does
// not care which one it is: WhatsApp today, SMS when a Sender is registered for
// it, and the only difference either way is which adapter the container wired.
// Everything below — the durable job, the dedupe rule, the refusal to log a code
// — is identical, which is the reason this is a second constructor and not a
// second type.
func NewPhoneCodeSender(notifier *Notifier, dispatcher *queueUsecase.Dispatcher, channel domain.Channel) *CodeSender {
	return &CodeSender{notifier: notifier, dispatcher: dispatcher, channel: channel}
}

// Send queues the code and announces the job.
//
// Unlike the purchase messages, this is NOT raised inside somebody else's
// transaction: there is no money to commit alongside it, and the caller is a
// person waiting on a screen. The job is written and published immediately.
func (s *CodeSender) Send(ctx context.Context, destination, code string, purpose authdomain.Purpose) error {
	if s == nil || s.notifier == nil {
		// No provider. The honest answer is that signing in does not work right
		// now, not a code nobody will receive, and not a code in the log.
		return authdomain.ErrDeliveryUnavailable
	}

	request := domain.Request{
		Channel:   s.channel,
		Template:  domain.TemplateSignInCode,
		Recipient: s.recipient(destination),
		Data: map[string]any{
			"Code":            code,
			"ValidForMinutes": strconv.Itoa(int(authdomain.CodeTTL / time.Minute)),
			"Purpose":         string(purpose),
		},
		// Keyed on the code AND the channel. Two codes for one address are two
		// different codes and both must be sent, so the code is what makes them
		// distinct; the channel is there because the same code deliberately sent
		// by two routes is two messages, and a key without it would silently drop
		// the second.
		DedupeKey: string(domain.TemplateSignInCode) + ":" + string(s.channel) + ":" + code,
	}
	if s.channel == domain.ChannelEmail {
		// Meaningful to email and ignored everywhere else. It carries the code by
		// design — a subject line that hides it makes the reader open the message
		// to read six digits — and that is a property of INBOXES, where the
		// preview is the feature. It would be a leak on a channel whose wording
		// somebody else controls.
		request.Subject = "Seu código de acesso: " + code
	}

	job, err := s.notifier.Enqueue(ctx, nil, request)
	if err != nil {
		return err
	}
	if job == nil {
		// The notifier declined it: no sender for the channel, or a
		// duplicate. A sign-in that queued nothing has not been sent, and
		// saying so beats a screen waiting for a code that is not coming.
		return authdomain.ErrDeliveryUnavailable
	}
	s.dispatcher.Dispatch(ctx, job)
	return nil
}

// recipient addresses the code for whichever channel carries it.
//
// One place rather than a branch at each call site, so a channel added to the
// domain is handled here or nowhere — and "nowhere" is safe: Recipient.Address
// returns "" for a channel it cannot address, and the Service parks the job
// instead of sending a code into the void.
func (s *CodeSender) recipient(destination string) domain.Recipient {
	if s.channel.Phone() {
		return domain.Recipient{Phone: destination}
	}
	return domain.Recipient{Email: destination}
}
