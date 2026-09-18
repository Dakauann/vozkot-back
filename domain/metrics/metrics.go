// Package metrics is what the rest of the system reports to, without knowing
// that Prometheus exists.
//
// The interfaces here are declared for their CONSUMERS and kept deliberately
// tiny: a use case that parks a job needs one method, not a monitoring library.
// Everything is optional at every call site, so a deployment with no metrics
// runs unchanged and a test can pass nil.
//
// THE RULE THAT KEEPS THIS AFFORDABLE: no identifier is ever a label. Not an
// order id, not an organiser id, not a buyer's address. Each distinct label
// value is a separate time series held in memory for as long as it is retained,
// so one label carrying order ids is an unbounded series count and an
// out-of-memory Prometheus. Per-entity facts belong in PostgreSQL and on the
// report pages, which is where this system already answers them. What belongs
// here is a bounded set: a status, a job type, an HTTP method.
package metrics

import "time"

// HTTPRecorder is the request-level view: how many, how slow, how many at once.
type HTTPRecorder interface {
	IncHTTPRequests(method, path, status string)
	ObserveHTTPLatency(method, path, status string, elapsed time.Duration)
	IncHTTPInFlight(method, path string)
	DecHTTPInFlight(method, path string)
}

// QueueRecorder is the background work, and it is the one an operator watches
// on event night.
//
// A parked job is money or stock that needs a person: a create_charge that
// never issued a PIX is a buyer holding a reservation they cannot pay for, and
// a send_notification is a buyer with no ticket in their inbox. Until this
// existed, the only trace was a log line nobody was reading at 22:00.
type QueueRecorder interface {
	// SetQueueJobs publishes the depth for one status: pending, processing,
	// dead. A gauge rather than a counter, because the question is "how many
	// are there now", not "how many have ever been".
	SetQueueJobs(status string, count int64)
	// IncJobParked counts a job that exhausted its attempts, by type. A counter
	// as well as the gauge above, because a job parked and cleaned up between
	// two scrapes would never appear in the gauge at all.
	IncJobParked(jobType string)
}

// OrderRecorder is the sale, reduced to the one question worth alerting on.
type OrderRecorder interface {
	// SetOrdersByStatus publishes how many orders sit in each status.
	//
	// The status that matters is refund_required: money taken for tickets that
	// no longer exist. Anything above zero is somebody owed their money back.
	SetOrdersByStatus(status string, count int64)
}

// RateLimitRecorder counts refusals, and failures to refuse.
type RateLimitRecorder interface {
	IncRateLimited(limiter, reason string)
}

// Why a request was not allowed through, as a bounded label.
//
// The two are opposites and must never be collapsed into one counter.
// Exceeded is the limiter working. Error is the limiter BROKEN, and because
// this system fails open, a broken limiter means traffic is passing
// unthrottled rather than being turned away. One is a busy afternoon; the
// other is the credential routes standing unguarded.
const (
	RateLimitReasonExceeded = "limit_exceeded"
	RateLimitReasonError    = "limiter_error"
)
