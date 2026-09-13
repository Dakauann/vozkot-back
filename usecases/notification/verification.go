package notification

import (
	"context"
	"log"
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
//   - Its dedupe key is the CHALLENGE id, not the address. Two codes requested
//     for one address are two different codes and both must be sent — a key on
//     the address would silently drop the second, leaving somebody holding a
//     code the server has already replaced.
//   - Nothing about it is logged beyond the fact that it happened. A log line
//     carrying the code is the code, in a system that ships logs somewhere
//     else and keeps them for a year.

// CodeSender queues verification codes for delivery.
type CodeSender struct {
	notifier   *Notifier
	dispatcher *queueUsecase.Dispatcher
}

func NewCodeSender(notifier *Notifier, dispatcher *queueUsecase.Dispatcher) *CodeSender {
	return &CodeSender{notifier: notifier, dispatcher: dispatcher}
}

// Send queues the code and announces the job.
//
// Unlike the purchase messages, this is NOT raised inside somebody else's
// transaction: there is no money to commit alongside it, and the caller is a
// person waiting on a screen. The job is written and published immediately.
func (s *CodeSender) Send(ctx context.Context, destination, code string, purpose authdomain.Purpose) error {
	if s == nil || s.notifier == nil {
		// No provider configured. In development that is the normal state, and
		// refusing to sign anybody in because email is not set up would make
		// the app unusable on a fresh clone — so the code goes to the log,
		// which is the one place it is acceptable precisely because there is
		// no mail provider to have sent it.
		log.Printf("auth: no mail provider configured; verification code for %s is %s", destination, code)
		return nil
	}

	job, err := s.notifier.Enqueue(ctx, nil, domain.Request{
		Channel:   domain.ChannelEmail,
		Template:  domain.TemplateSignInCode,
		Recipient: domain.Recipient{Email: destination},
		Subject:   "Seu código de acesso: " + code,
		Data: map[string]any{
			"Code":            code,
			"ValidForMinutes": strconv.Itoa(int(authdomain.CodeTTL / time.Minute)),
			"Purpose":         string(purpose),
		},
		// Keyed on the code itself rather than the address: two codes for one
		// address are two messages, and only a literal duplicate of the same
		// send should collapse.
		DedupeKey: string(domain.TemplateSignInCode) + ":" + code,
	})
	if err != nil {
		return err
	}
	s.dispatcher.Dispatch(ctx, job)
	return nil
}

// MockPhoneSender stands in for an SMS or WhatsApp provider.
//
// Deliberately a stub, and deliberately not silent: phone confirmation is wired
// end to end — the challenge, the rate limits, the attempt cap, the blind index,
// the verified-at timestamp — and only the last hop is missing. Swapping this
// for a real provider is one implementation of authdomain Sender and one line
// in the container.
//
// It logs the code so the flow can be walked in development. That is safe here
// for the same reason it is safe above and nowhere else: there is no provider,
// so the log is not a SECOND copy of a credential that also went somewhere.
type MockPhoneSender struct{}

func NewMockPhoneSender() *MockPhoneSender { return &MockPhoneSender{} }

func (m *MockPhoneSender) Send(_ context.Context, destination, code string, _ authdomain.Purpose) error {
	log.Printf("auth: [MOCK SMS] verification code for %s is %s", destination, code)
	return nil
}
