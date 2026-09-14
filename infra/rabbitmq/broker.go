// Package rabbitmq is the job transport.
//
// Topology, and why each piece is there:
//
//	exchange  vozkot.jobs        durable direct exchange
//	queue     vozkot.jobs.work   durable, one queue for every job type
//	exchange  vozkot.jobs.dlx    dead-letter exchange
//	queue     vozkot.jobs.dead   where rejected deliveries land
//
// One work queue rather than one per type: the consumer reads the job row
// anyway, the types share a worker pool, and per-type queues would let a burst
// of one type starve another only if they had separate consumers, which is a
// scaling decision, not a default.
//
// Publisher confirms are on. Without them a publish is fire-and-forget, and the
// caller cannot know whether the broker accepted the message, which turns the
// polling fallback from a safety net into the primary path without anyone
// noticing.
package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"vozkot/domain/queue"
)

const (
	defaultExchangeName     = "vozkot.jobs"
	defaultDeadExchangeName = "vozkot.jobs.dlx"
	defaultWorkQueueName    = "vozkot.jobs.work"
	defaultDeadQueueName    = "vozkot.jobs.dead"
	defaultRoutingKey       = "job"
)

// Broker is a RabbitMQ connection with one publishing channel and any number of
// consumers.
type Broker struct {
	url string

	exchangeName     string
	deadExchangeName string
	workQueueName    string
	deadQueueName    string
	routingKey       string
	queueType        string
	durable          bool
	autoDelete       bool
	exclusive        bool

	mu         sync.RWMutex
	connection *amqp.Connection
	publishCh  *amqp.Channel

	// prefetch bounds how many unacknowledged deliveries one consumer holds.
	prefetch int
	closed   bool
}

var _ queue.Broker = (*Broker)(nil)

type Option func(*Broker)

// WithPrefetch sets the per-consumer QoS.
func WithPrefetch(prefetch int) Option {
	return func(b *Broker) {
		if prefetch > 0 {
			b.prefetch = prefetch
		}
	}
}

// WithNamespace isolates one topology from another on the same RabbitMQ
// virtual host. Production normally uses the defaults; integration tests use
// a unique namespace so concurrent test packages cannot consume each other's
// messages.
func WithNamespace(namespace string) Option {
	return func(b *Broker) {
		namespace = strings.Trim(strings.TrimSpace(namespace), ".")
		if namespace == "" {
			return
		}
		suffix := "." + namespace
		b.exchangeName += suffix
		b.deadExchangeName += suffix
		b.workQueueName += suffix
		b.deadQueueName += suffix
	}
}

// WithEphemeralTopology makes queues exclusive to this connection and removes
// them when it closes. It is intended for tests, never for production jobs.
func WithEphemeralTopology() Option {
	return func(b *Broker) {
		b.durable = false
		b.autoDelete = true
		b.exclusive = true
	}
}

// WithQueueType selects the durable work-queue implementation. "quorum" is
// the production HA choice; the empty/default value keeps a classic queue for
// single-node development brokers.
func WithQueueType(queueType string) Option {
	return func(b *Broker) {
		queueType = strings.ToLower(strings.TrimSpace(queueType))
		if queueType == "quorum" {
			b.queueType = queueType
		}
	}
}

// Connect dials RabbitMQ and declares the topology.
func Connect(url string, opts ...Option) (*Broker, error) {
	broker := &Broker{
		url:              url,
		prefetch:         16,
		exchangeName:     defaultExchangeName,
		deadExchangeName: defaultDeadExchangeName,
		workQueueName:    defaultWorkQueueName,
		deadQueueName:    defaultDeadQueueName,
		routingKey:       defaultRoutingKey,
		durable:          true,
	}
	for _, opt := range opts {
		opt(broker)
	}
	if err := broker.connect(); err != nil {
		return nil, err
	}
	go broker.watch()
	return broker, nil
}

func (b *Broker) connect() error {
	connection, err := amqp.DialConfig(b.url, amqp.Config{
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		return fmt.Errorf("rabbitmq: dial: %w", err)
	}

	channel, err := connection.Channel()
	if err != nil {
		connection.Close()
		return fmt.Errorf("rabbitmq: open channel: %w", err)
	}
	if err := b.declareTopology(channel); err != nil {
		connection.Close()
		return err
	}
	// Confirms turn a publish into something that can be waited on and
	// therefore reported.
	if err := channel.Confirm(false); err != nil {
		connection.Close()
		return fmt.Errorf("rabbitmq: enable publisher confirms: %w", err)
	}

	b.mu.Lock()
	b.connection = connection
	b.publishCh = channel
	b.mu.Unlock()
	return nil
}

func (b *Broker) declareTopology(channel *amqp.Channel) error {
	var deadArguments amqp.Table
	workArguments := amqp.Table{"x-dead-letter-exchange": b.deadExchangeName}
	if b.queueType == "quorum" && !b.exclusive {
		deadArguments = amqp.Table{"x-queue-type": "quorum"}
		workArguments["x-queue-type"] = "quorum"
	}
	if err := channel.ExchangeDeclare(b.deadExchangeName, "fanout", b.durable, b.autoDelete, false, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: declare dead-letter exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(b.deadQueueName, b.durable, b.autoDelete, b.exclusive, false, deadArguments); err != nil {
		return fmt.Errorf("rabbitmq: declare dead-letter queue: %w", err)
	}
	if err := channel.QueueBind(b.deadQueueName, "", b.deadExchangeName, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: bind dead-letter queue: %w", err)
	}

	if err := channel.ExchangeDeclare(b.exchangeName, "direct", b.durable, b.autoDelete, false, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: declare exchange: %w", err)
	}
	// A delivery this consumer rejects outright goes to the dead-letter queue
	// instead of looping between broker and worker forever.
	if _, err := channel.QueueDeclare(b.workQueueName, b.durable, b.autoDelete, b.exclusive, false, workArguments); err != nil {
		return fmt.Errorf("rabbitmq: declare work queue: %w", err)
	}
	if err := channel.QueueBind(b.workQueueName, b.routingKey, b.exchangeName, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: bind work queue: %w", err)
	}
	return nil
}

// watch reconnects when the connection drops.
//
// A broker restart, a network blip or a failover must not leave the application
// permanently unable to dispatch work; until it reconnects, the database poller
// keeps the system running at a slower cadence.
func (b *Broker) watch() {
	for {
		b.mu.RLock()
		connection := b.connection
		closed := b.closed
		b.mu.RUnlock()
		if closed || connection == nil {
			return
		}

		reason := <-connection.NotifyClose(make(chan *amqp.Error, 1))
		b.mu.RLock()
		closed = b.closed
		b.mu.RUnlock()
		if closed {
			return
		}
		log.Printf("rabbitmq: connection lost (%v); reconnecting", reason)

		for attempt := 1; ; attempt++ {
			time.Sleep(backoff(attempt))
			b.mu.RLock()
			closed := b.closed
			b.mu.RUnlock()
			if closed {
				return
			}
			if err := b.connect(); err != nil {
				log.Printf("rabbitmq: reconnect attempt %d: %v", attempt, err)
				continue
			}
			log.Printf("rabbitmq: reconnected")
			break
		}
	}
}

func backoff(attempt int) time.Duration {
	delay := time.Duration(attempt) * time.Second
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	return delay
}

// Publish sends one job reference and waits for the broker to confirm it.
//
// Persistent delivery mode plus a durable queue is what survives a broker
// restart; without both, an accepted message can still vanish.
func (b *Broker) Publish(ctx context.Context, message queue.Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}

	b.mu.RLock()
	channel := b.publishCh
	if channel == nil {
		b.mu.RUnlock()
		return errors.New("rabbitmq: not connected")
	}

	// A deferred confirmation is tied to this publish's sequence number. A
	// shared NotifyPublish channel is not: concurrent callers can consume one
	// another's ack/nack, and RabbitMQ does not guarantee confirmations arrive
	// in publish order.
	confirmation, err := channel.PublishWithDeferredConfirmWithContext(ctx, b.exchangeName, b.routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    message.JobID,
		Type:         message.Type,
		Timestamp:    time.Now().UTC(),
		Body:         body,
	})
	b.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("rabbitmq: publish: %w", err)
	}
	if confirmation == nil {
		return errors.New("rabbitmq: publisher confirmations are not enabled")
	}
	confirmCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	acknowledged, err := confirmation.WaitContext(confirmCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return errors.New("rabbitmq: timed out waiting for publisher confirmation")
		}
		return err
	}
	if !acknowledged {
		return errors.New("rabbitmq: broker refused the message")
	}
	return nil
}

// Consume delivers messages to handler until ctx is cancelled.
//
// Deliveries run CONCURRENTLY, and prefetch is the bound: RabbitMQ never has
// more than `prefetch` unacknowledged deliveries outstanding to this consumer,
// and each is acknowledged only when its handler returns, so the handlers in
// flight can never exceed it. A job here is mostly a round trip to a payment
// provider, hundreds of milliseconds of waiting, and running those one after
// another made prefetch a buffer instead of the throughput knob it is meant to
// be: two workers at 300 ms were seven charges a second.
//
// Every delivery is acknowledged after the handler returns, including when it
// returns an error: retry state lives in the job row, with its own attempt
// counter and backoff, so redelivering from the broker as well would multiply
// attempts and ignore the schedule. A delivery that cannot even be decoded is
// rejected to the dead-letter queue, because no number of retries will parse it.
//
// Consume returns only when nothing is in flight, so a caller that cancels the
// context and waits for it knows every started handler has finished.
func (b *Broker) Consume(ctx context.Context, handler func(ctx context.Context, message queue.Message) error) error {
	b.mu.RLock()
	connection := b.connection
	b.mu.RUnlock()
	if connection == nil {
		return errors.New("rabbitmq: not connected")
	}

	channel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("rabbitmq: open consumer channel: %w", err)
	}
	defer channel.Close()

	if err := channel.Qos(b.prefetch, 0, false); err != nil {
		return fmt.Errorf("rabbitmq: set prefetch: %w", err)
	}

	deliveries, err := channel.Consume(b.workQueueName, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("rabbitmq: consume: %w", err)
	}

	// Waited on before the channel closes (defers run last-in first-out), so
	// every acknowledgement goes out on an open channel.
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return nil
			}
			var message queue.Message
			if err := json.Unmarshal(delivery.Body, &message); err != nil {
				log.Printf("rabbitmq: undecodable delivery sent to the dead-letter queue: %v", err)
				_ = delivery.Reject(false)
				continue
			}
			inFlight.Add(1)
			go func(delivery amqp.Delivery, message queue.Message) {
				defer inFlight.Done()
				if err := handler(ctx, message); err != nil {
					log.Printf("rabbitmq: job %s failed: %v", message.JobID, err)
				}
				if err := delivery.Ack(false); err != nil {
					log.Printf("rabbitmq: ack %s: %v", message.JobID, err)
				}
			}(delivery, message)
		}
	}
}

func (b *Broker) Close() error {
	b.mu.Lock()
	b.closed = true
	connection := b.connection
	b.connection = nil
	b.publishCh = nil
	b.mu.Unlock()

	if connection == nil {
		return nil
	}
	return connection.Close()
}

// QueueDepth reports how many messages are waiting, for the health endpoint.
func (b *Broker) QueueDepth() (int, error) {
	b.mu.RLock()
	channel := b.publishCh
	b.mu.RUnlock()
	if channel == nil {
		return 0, errors.New("rabbitmq: not connected")
	}
	state, err := channel.QueueInspect(b.workQueueName)
	if err != nil {
		return 0, err
	}
	return state.Messages, nil
}
