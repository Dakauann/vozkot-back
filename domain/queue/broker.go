package queue

import "context"

// The broker is the TRANSPORT. The job table is the LEDGER.
//
// They are not alternatives, and using only one of them breaks in a way that
// matters here:
//
//   - Broker only. The message and the order cannot be committed together. A
//     crash between the two either loses the work for an order that exists, or
//     announces work for an order that rolled back. Every fix for that
//     reintroduces a table.
//   - Table only. Correct, but dispatch becomes polling: a buyer waits up to
//     one poll interval for a PIX code, and a thousand idle workers hammer the
//     database to discover there is nothing to do.
//
// So the job row is written inside the business transaction, and the broker is
// told about it immediately after that transaction commits. The broker makes
// delivery instant; the row makes it exactly-once, because a consumer claims
// the row before doing anything and a redelivered message finds it already
// claimed. If a publish is lost, the poller still finds the row, the system
// degrades to the slower path instead of losing work.

// Message is the envelope on the wire. It carries ids only: the consumer reads
// the current row, so a message delivered an hour late acts on today's state
// rather than on a snapshot of the world when it was published.
type Message struct {
	JobID string `json:"jobId"`
	Type  string `json:"type"`
}

// Publisher hands a job to the broker. It is called AFTER the transaction that
// wrote the job row commits, never inside it.
type Publisher interface {
	Publish(ctx context.Context, message Message) error
}

// Consumer delivers messages to a handler until the context is cancelled.
//
// Implementations must acknowledge a message only after the handler returns,
// and must bound how many are in flight per consumer; an unbounded prefetch is
// how one slow worker takes a whole queue hostage.
type Consumer interface {
	Consume(ctx context.Context, handler func(ctx context.Context, message Message) error) error
}

// Broker is both halves, which is what an adapter implements.
type Broker interface {
	Publisher
	Consumer
	Close() error
}
