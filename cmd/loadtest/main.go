// Command loadtest drives the purchase system the way an on-sale does, then
// audits the result the way an accountant would.
//
// It runs the REAL pipeline in one process: checkout, the PostgreSQL job
// ledger, RabbitMQ transport when configured, the Redis cache and rate limiter
// when configured, the Mercado Pago adapter, settlement, against a stub of
// Mercado Pago's HTTP API that misbehaves on purpose: it answers slowly, fails
// a share of requests with 5xx, delivers every webhook more than once, and
// signs each one with the real HMAC so the real verifier has to accept it.
//
// When the storm is over it checks the invariants that money depends on and
// exits non-zero if any is broken:
//
//   - no tier has sold + reserved above its capacity
//   - every paid order committed exactly its quantity
//   - every approved charge belongs to exactly one order
//   - no order was charged twice
//   - retried checkouts produced one order, not two
//   - the queue drained, with nothing parked
//
// Usage:
//
//	go run ./cmd/loadtest -orders 20000 -buyers 200 -tiers 20 -capacity 500
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	webhooksHTTP "vozkot/delivery/http/webhooks"
	eventdomain "vozkot/domain/event"
	orderdomain "vozkot/domain/order"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/config"
	"vozkot/infra/database"
	"vozkot/infra/mercadopago"
	"vozkot/infra/rabbitmq"
	redisCache "vozkot/infra/redis"
	eventRepository "vozkot/infra/repositories/event"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/uow"
	checkoutUsecase "vozkot/usecases/checkout"
	paymentUsecase "vozkot/usecases/payment"
	queueUsecase "vozkot/usecases/queue"
)

const webhookSecret = "loadtest-webhook-secret"

type options struct {
	orders          int
	buyers          int
	tiers           int
	capacity        int
	maxQuantity     int
	workers         int
	approveRate     float64
	providerFailure float64
	providerLatency time.Duration
	duplicateHooks  int
	webhookLoss     float64
	retryShare      float64
	drainTimeout    time.Duration
	keep            bool
	// overHTTP drives the REAL router: TLS, session lookup, idempotency
	// claim, the admission bulkhead, JSON, instead of calling the use case.
	//
	// The two numbers are not comparable and the difference is the point. A
	// use-case run measures how fast PostgreSQL can arbitrate inventory; a
	// replica's actual ceiling is CHECKOUT_MAX_IN_FLIGHT divided by the latency
	// of a whole request, which is several times what Start alone costs. Only
	// the second number belongs in a capacity plan.
	overHTTP    bool
	maxInFlight int
	// dbConns bounds the connection pool. It is deliberately SMALLER than the
	// number of buyers: under a real on-sale the pool is the bottleneck that
	// turns a stampede into a queue, and the property being tested is that
	// requests wait on it rather than the database refusing connections.
	dbConns int
}

func main() {
	var opts options
	flag.IntVar(&opts.orders, "orders", 5000, "checkout attempts to make")
	flag.IntVar(&opts.buyers, "buyers", 100, "concurrent buyers")
	flag.IntVar(&opts.tiers, "tiers", 10, "ticket tiers on sale")
	flag.IntVar(&opts.capacity, "capacity", 300, "capacity per tier (deliberately below demand)")
	flag.IntVar(&opts.maxQuantity, "max-quantity", 4, "largest quantity a buyer asks for")
	flag.IntVar(&opts.workers, "workers", 4, "queue workers")
	flag.Float64Var(&opts.approveRate, "approve-rate", 0.85, "share of charges the provider approves; the rest are rejected or expire")
	flag.Float64Var(&opts.providerFailure, "provider-failure", 0.05, "share of provider calls that fail with a 5xx and must be retried")
	flag.DurationVar(&opts.providerLatency, "provider-latency", 20*time.Millisecond, "simulated provider round trip")
	flag.IntVar(&opts.duplicateHooks, "duplicate-webhooks", 3, "how many times each webhook is delivered")
	flag.Float64Var(&opts.webhookLoss, "webhook-loss", 0.01, "share of payments whose webhook deliveries are all lost and must be reconciled")
	flag.Float64Var(&opts.retryShare, "retry-share", 0.10, "share of checkouts retried with the same idempotency key")
	flag.DurationVar(&opts.drainTimeout, "drain-timeout", 5*time.Minute, "how long to wait for the queue to empty")
	flag.BoolVar(&opts.keep, "keep", false, "keep the generated rows for inspection")
	flag.IntVar(&opts.dbConns, "db-conns", 40, "database connection pool size; keep it under the server's max_connections")
	flag.BoolVar(&opts.overHTTP, "http", false, "drive the real HTTP router instead of the use case, and report a per-replica ceiling")
	flag.IntVar(&opts.maxInFlight, "max-in-flight", 20, "checkout admission bulkhead, with -http; matches CHECKOUT_MAX_IN_FLIGHT")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Println("godotenv: no .env file found, continuing with the environment")
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}

	if err := run(cfg, opts); err != nil {
		log.Fatalf("LOAD TEST FAILED: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The provider stub: Mercado Pago's API, with the bad days included.

type stubProvider struct {
	mu       sync.Mutex
	payments map[string]map[string]any
	// byKey indexes payments by idempotency key. The real provider has an
	// index here; a scan of every payment ever created, under this lock, would
	// make the STUB the bottleneck the run is trying to measure.
	byKey           map[string]map[string]any
	next            int64
	created         atomic.Int64
	fetched         atomic.Int64
	failures        atomic.Int64
	opts            options
	random          *mathrand.Rand
	webhookTo       string
	client          *http.Client
	delivered       atomic.Int64
	webhookAttempts atomic.Int64
	webhookFailures atomic.Int64
}

func newStubProvider(opts options) *stubProvider {
	return &stubProvider{
		payments: map[string]map[string]any{},
		byKey:    map[string]map[string]any{},
		next:     5_000_000_000,
		opts:     opts,
		random:   mathrand.New(mathrand.NewSource(time.Now().UnixNano())),
	}
}

func (p *stubProvider) shouldFail() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.random.Float64() < p.opts.providerFailure
}

func (p *stubProvider) handler() http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		time.Sleep(p.opts.providerLatency)
		response.Header().Set("Content-Type", "application/json")

		if p.shouldFail() {
			p.failures.Add(1)
			response.WriteHeader(http.StatusBadGateway)
			_, _ = response.Write([]byte(`{"message":"provider temporarily unavailable"}`))
			return
		}

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/payments":
			var body mercadopago.CreatePaymentRequest
			_ = json.NewDecoder(request.Body).Decode(&body)

			p.mu.Lock()
			// The provider's own idempotency: the same X-Idempotency-Key
			// returns the same payment, exactly as Mercado Pago does.
			key := request.Header.Get("X-Idempotency-Key")
			if existing, ok := p.byKey[key]; ok {
				p.mu.Unlock()
				_ = json.NewEncoder(response).Encode(existing)
				return
			}
			p.next++
			id := strconv.FormatInt(p.next, 10)
			payment := map[string]any{
				"id":                 p.next,
				"idempotency_key":    key,
				"status":             mercadopago.StatusPending,
				"status_detail":      "pending_waiting_transfer",
				"external_reference": body.ExternalReference,
				"payment_method_id":  "pix",
				"transaction_amount": body.TransactionAmount,
				"point_of_interaction": map[string]any{"transaction_data": map[string]any{
					"qr_code": "00020126-" + id, "qr_code_base64": "aGVsbG8=",
				}},
			}
			p.payments[id] = payment
			p.byKey[key] = payment
			p.mu.Unlock()
			p.created.Add(1)
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(payment)

		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/payments/"):
			p.fetched.Add(1)
			id := strings.TrimPrefix(request.URL.Path, "/v1/payments/")
			p.mu.Lock()
			payment, ok := p.payments[id]
			p.mu.Unlock()
			if !ok {
				response.WriteHeader(http.StatusNotFound)
				_, _ = response.Write([]byte(`{"message":"Payment not found","status":404}`))
				return
			}
			_ = json.NewEncoder(response).Encode(payment)

		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}
}

// settleEverything decides each payment's fate and fires the webhooks, each
// one delivered several times, as the real provider does.
func (p *stubProvider) settleEverything() (approved, rejected, expired int) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.payments))
	for id := range p.payments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		payment := p.payments[id]
		roll := p.random.Float64()
		switch {
		case roll < p.opts.approveRate:
			payment["status"] = mercadopago.StatusApproved
			payment["status_detail"] = mercadopago.DetailAccredited
			approved++
		case roll < p.opts.approveRate+(1-p.opts.approveRate)/2:
			payment["status"] = mercadopago.StatusRejected
			payment["status_detail"] = "cc_rejected_other_reason"
			rejected++
		default:
			payment["status"] = mercadopago.StatusCancelled
			payment["status_detail"] = mercadopago.DetailExpired
			expired++
		}
	}
	p.mu.Unlock()

	var wait sync.WaitGroup
	limiter := make(chan struct{}, 32)
	for _, id := range ids {
		p.mu.Lock()
		loseAll := p.random.Float64() < p.opts.webhookLoss
		p.mu.Unlock()
		if loseAll {
			p.webhookAttempts.Add(int64(p.opts.duplicateHooks))
			p.webhookFailures.Add(int64(p.opts.duplicateHooks))
			continue
		}
		for copyIndex := 0; copyIndex < p.opts.duplicateHooks; copyIndex++ {
			wait.Add(1)
			limiter <- struct{}{}
			go func(id string) {
				defer wait.Done()
				defer func() { <-limiter }()
				p.deliverWebhook(id)
			}(id)
		}
	}
	wait.Wait()
	return approved, rejected, expired
}

func (p *stubProvider) deliverWebhook(paymentID string) {
	p.webhookAttempts.Add(1)
	body := fmt.Sprintf(`{"id":%d,"type":"payment","action":"payment.updated","data":{"id":"%s"}}`, time.Now().UnixNano(), paymentID)
	request, err := http.NewRequest(http.MethodPost, p.webhookTo+"?data.id="+paymentID+"&type=payment", strings.NewReader(body))
	if err != nil {
		p.webhookFailures.Add(1)
		return
	}
	requestID := randomID()
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-request-id", requestID)
	request.Header.Set("x-signature", mercadopago.SignForTesting(paymentID, requestID, webhookSecret, time.Now()))
	response, err := p.client.Do(request)
	if err != nil {
		p.webhookFailures.Add(1)
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		p.delivered.Add(1)
		return
	}
	p.webhookFailures.Add(1)
}

func (p *stubProvider) approvedIDs() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := map[string]bool{}
	for id, payment := range p.payments {
		if payment["status"] == mercadopago.StatusApproved {
			ids[id] = true
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// The run.

type latencies struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.samples = append(l.samples, d)
	l.mu.Unlock()
}

func (l *latencies) percentile(p float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), l.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func run(cfg config.Config, opts options) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- infrastructure -----------------------------------------------------
	migrationDB, err := database.NewMigrationDatabase(ctx, cfg.Database)
	if err != nil {
		return err
	}
	if err := database.RunMigrations(ctx, migrationDB); err != nil {
		return err
	}
	if sqlDB, err := migrationDB.DB(); err == nil {
		sqlDB.Close()
	}
	cfg.Database.MaxOpenConns = opts.dbConns
	cfg.Database.MaxIdleConns = opts.dbConns
	log.Printf("database pool: %d connection(s) shared by %d buyers and %d workers", opts.dbConns, opts.buyers, opts.workers)
	db, err := database.NewApplicationDatabase(ctx, cfg.Database)
	if err != nil {
		return err
	}
	// The invariant logger still observes every trace, but prints only actual
	// SQL errors. Lock contention is expected in this stress tool; dumping one
	// "slow query" per checkout changes the result by benchmarking the console.
	refused := &statementErrors{Interface: db.Logger.LogMode(gormlogger.Error)}
	db.Logger = refused

	var publisher queuedomain.Publisher
	var consumer queuedomain.Consumer
	if cfg.Broker.Enabled() {
		broker, err := rabbitmq.Connect(cfg.Broker.URL, rabbitmq.WithPrefetch(cfg.Broker.Prefetch),
			rabbitmq.WithQueueType(cfg.Broker.QueueType))
		if err != nil {
			return fmt.Errorf("rabbitmq: %w", err)
		}
		defer broker.Close()
		publisher, consumer = broker, broker
		log.Printf("transport: RabbitMQ")
	} else {
		log.Printf("transport: database poller only (RABBITMQ_URL is not set)")
	}

	var tickets ticketdomain.Repository = ticketRepository.NewTicketRepository(db)
	var orders orderdomain.Repository = orderRepository.NewOrderRepository(db)
	if cfg.Cache.Enabled() {
		cache, err := redisCache.Connect(ctx, cfg.Cache.URL, cfg.Cache.KeyPrefix+":loadtest")
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		defer cache.Close()
		tickets = ticketRepository.NewCachedTicketRepository(tickets, cache, ticketRepository.TicketTTL)
		orders = orderRepository.NewCachedOrderRepository(orders, cache, orderRepository.OrderTTL)
		log.Printf("cache: Redis")
	} else {
		log.Printf("cache: none (REDIS_URL is not set)")
	}

	jobs := queueRepository.NewJobRepository(db)
	dispatcher := queueUsecase.NewDispatcher(publisher)
	unit := uow.NewRunner(db)

	// --- the provider and the webhook endpoint ------------------------------
	provider := newStubProvider(opts)
	providerServer := newHTTP2Server(provider.handler())
	defer providerServer.Close()

	webhookMux := http.NewServeMux()
	webhooksHTTP.NewMercadoPagoHandler(jobs, dispatcher, webhookSecret, 0).Register(webhookMux)
	webhookServer := newHTTP2Server(webhookMux)
	defer webhookServer.Close()
	provider.webhookTo = webhookServer.URL + "/webhooks/mercadopago"
	provider.client = webhookServer.Client()
	provider.client.Timeout = 10 * time.Second

	providerHTTP := providerServer.Client()
	providerHTTP.Timeout = 30 * time.Second
	gateway := mercadopago.NewGateway(mercadopago.NewClient("TEST-loadtest", providerServer.URL,
		mercadopago.WithNotificationURL(provider.webhookTo), mercadopago.WithHTTPClient(providerHTTP)))

	// Hold limits are set generously rather than disabled: a run has one account
	// per buyer making far more purchases than a person ever would, so a
	// production limit would refuse most of the storm and measure nothing. A
	// ceiling above what the run can reach still exercises the per-buyer
	// advisory lock and the count on EVERY checkout, which is what needs
	// measuring; the limit's own behaviour is tested where it lives.
	perBuyer := opts.orders/max(opts.buyers, 1) + 1
	holdLimits := orderdomain.HoldLimits{Orders: perBuyer * 2}
	// nil buyers: the harness has no profiles to snapshot, and the audience
	// report is not what it measures. No fee either: the harness audits that
	// every centavo charged is a centavo owed, and a commission on top would
	// be a second number to reconcile for no gain.
	checkout := checkoutUsecase.NewService(unit, orders, tickets, nil, dispatcher, checkoutUsecase.Settings{
		HoldFor:     30 * time.Minute,
		CartHoldFor: 30 * time.Minute,
		HoldLimits:  holdLimits,
	})
	// nil notifications: the harness measures the purchase path, and mailing a
	// hundred thousand synthetic buyers is neither wanted nor free.
	payments := paymentUsecase.NewService(unit, orders, gateway, jobs, dispatcher, nil)

	// --- workers --------------------------------------------------------------
	workerCtx, stopWorkers := context.WithCancel(ctx)
	// Deferred as well as called explicitly below: an early return on a failed
	// drain must not leave the workers running.
	defer stopWorkers()
	var workersDone sync.WaitGroup
	for index := 0; index < opts.workers; index++ {
		options := []queueUsecase.Option{queueUsecase.WithBatchSize(20), queueUsecase.WithInterval(200 * time.Millisecond)}
		if consumer != nil {
			options = append(options, queueUsecase.WithBroker(consumer))
		}
		worker := queueUsecase.NewWorker(jobs, options...)
		worker.Handle(queuedomain.TypeCreateCharge, func(ctx context.Context, job queuedomain.Job) error {
			payload, err := queueUsecase.Decode[queuedomain.CreateChargePayload](job)
			if err != nil {
				return err
			}
			return payments.CreateCharge(ctx, payload.OrderID)
		})
		worker.Handle(queuedomain.TypeSyncPayment, func(ctx context.Context, job queuedomain.Job) error {
			payload, err := queueUsecase.Decode[queuedomain.SyncPaymentPayload](job)
			if err != nil {
				return err
			}
			if payload.PaymentID != "" {
				return payments.SyncPayment(ctx, payload.PaymentID)
			}
			return payments.SyncOrder(ctx, payload.OrderID)
		})
		worker.Handle(queuedomain.TypeExpireHolds, func(ctx context.Context, job queuedomain.Job) error {
			payload, err := queueUsecase.Decode[queuedomain.SyncPaymentPayload](job)
			if err != nil {
				return err
			}
			if payload.OrderID != "" {
				return checkout.ExpireHold(ctx, payload.OrderID)
			}
			_, err = checkout.ExpireHolds(ctx, 500)
			return err
		})
		worker.Handle(queuedomain.TypeReconcile, func(ctx context.Context, _ queuedomain.Job) error {
			_, err := payments.Reconcile(ctx, 500)
			return err
		})
		workersDone.Add(1)
		go func() {
			defer workersDone.Done()
			worker.Run(workerCtx)
		}()
	}

	// --- seed -----------------------------------------------------------------
	runID := randomID()
	ownerID := "usr_load_" + runID
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Load Test', ?, 'x', 'user', 0, NOW(), NOW())`, ownerID, ownerID+"@vozkot.test").Error; err != nil {
		return err
	}
	// One event, many tiers; the shape a real on-sale has, and the one the
	// inventory contention this harness measures actually happens in.
	happening, err := eventdomain.New("evt_load_"+runID, ownerID, eventdomain.Draft{
		Name:     "Load Test " + runID,
		Category: eventdomain.CategoryFestasShows,
		Location: eventdomain.Location{Venue: "Arena", City: "Fortaleza", UF: "CE"},
		StartsAt: time.Now().Add(720 * time.Hour),
		Status:   eventdomain.StatusPublished,
	}, time.Now())
	if err != nil {
		return err
	}
	if err := eventRepository.NewEventRepository(db).Create(ctx, happening); err != nil {
		return err
	}

	tierIDs := make([]string, 0, opts.tiers)
	for index := 0; index < opts.tiers; index++ {
		ticket, err := ticketdomain.New(fmt.Sprintf("tkt_load_%s_%d", runID, index), ownerID, ticketdomain.Draft{
			EventID:    happening.ID,
			Title:      fmt.Sprintf("Tier %d", index),
			PriceCents: 12000,
			Quantity:   opts.capacity,
			Status:     ticketdomain.StatusOnSale,
		}, time.Now())
		if err != nil {
			return err
		}
		if err := tickets.Create(ctx, ticket); err != nil {
			return err
		}
		tierIDs = append(tierIDs, ticket.ID)
	}
	if !opts.keep {
		defer cleanup(db, runID, ownerID)
	}

	log.Printf("seeded %d tier(s) x %d tickets = %d tickets for %d checkout attempts by %d buyers",
		opts.tiers, opts.capacity, opts.tiers*opts.capacity, opts.orders, opts.buyers)

	// --- how the storm makes a checkout ---------------------------------------
	//
	// Either straight into the use case, or through the real router. The second
	// is the only one whose throughput belongs in a capacity plan.
	var storm fleet = &useCaseFleet{service: checkout, buyerID: ownerID}
	if opts.overHTTP {
		httpFleet, err := newHTTPFleet(ctx, db, cfg, opts, runID, checkout, payments)
		if err != nil {
			return fmt.Errorf("http harness: %w", err)
		}
		storm = httpFleet
		if !opts.keep {
			defer cleanupHTTPAccounts(db, runID)
		}
		log.Printf("driving: the real HTTP router over TLS; bulkhead %d in flight per replica, %d account(s) with sessions",
			opts.maxInFlight, opts.buyers)
		log.Printf("NOTE: the per-account checkout rate limit is off in this mode; it would refuse a storm this dense before the bulkhead saw it")
	} else {
		log.Printf("driving: the checkout use case directly, NOT a per-replica HTTP number; run with -http for that")
	}
	defer storm.close()

	// --- the storm ------------------------------------------------------------
	var created, soldOut, replayed, failed, shed, throttled atomic.Int64
	var checkoutLatency latencies
	orderIDs := sync.Map{}
	attempts := make(chan int, opts.orders)
	for index := 0; index < opts.orders; index++ {
		attempts <- index
	}
	close(attempts)

	random := mathrand.New(mathrand.NewSource(42))
	var randomMu sync.Mutex
	pick := func(n int) int {
		randomMu.Lock()
		defer randomMu.Unlock()
		return random.Intn(n)
	}
	roll := func() float64 {
		randomMu.Lock()
		defer randomMu.Unlock()
		return random.Float64()
	}

	stormStarted := time.Now()
	var buyersDone sync.WaitGroup
	for buyer := 0; buyer < opts.buyers; buyer++ {
		buyersDone.Add(1)
		go func(buyer int) {
			defer buyersDone.Done()
			for index := range attempts {
				key := fmt.Sprintf("load-%s-%d", runID, index)
				ticketID := tierIDs[pick(len(tierIDs))]
				quantity := 1 + pick(opts.maxQuantity)

				started := time.Now()
				orderID, result, err := storm.checkout(ctx, buyer, key, ticketID, quantity)
				elapsed := time.Since(started)
				if result != outcomeShed && result != outcomeThrottled {
					// A shed request is refused before it does any work, in
					// microseconds. Mixing those into the percentiles would
					// drag p50 toward zero and report the speed of saying no as
					// if it were the speed of selling a ticket.
					checkoutLatency.add(elapsed)
				}

				switch result {
				case outcomeCreated:
					created.Add(1)
					orderIDs.Store(orderID, true)
					// A share of buyers retry with the same key: the network
					// dropped the response. The retry must come back with the
					// SAME order and must hold nothing extra, over HTTP that
					// is a replayed 201, in the use case a refused duplicate.
					if roll() < opts.retryShare {
						retryID, retryResult, _ := storm.checkout(ctx, buyer, key, ticketID, quantity)
						switch {
						case retryResult == outcomeCreated && retryID == orderID:
							replayed.Add(1)
						case retryResult == outcomeCreated:
							failed.Add(1)
							log.Printf("INVARIANT: a retried idempotency key created a second order (%s then %s)", orderID, retryID)
						default:
							replayed.Add(1)
						}
					}
				case outcomeSoldOut:
					soldOut.Add(1)
				case outcomeShed:
					shed.Add(1)
				case outcomeThrottled:
					throttled.Add(1)
				default:
					failed.Add(1)
					log.Printf("checkout failed: %v", err)
				}
			}
		}(buyer)
	}
	buyersDone.Wait()
	stormTook := time.Since(stormStarted)

	log.Printf("storm: %d order(s) opened, %d refused (sold out or limited), %d retried key(s) replayed, %d error(s) in %s",
		created.Load(), soldOut.Load(), replayed.Load(), failed.Load(), stormTook.Round(time.Millisecond))
	if opts.overHTTP {
		// The number a capacity plan may quote is what the replica ADMITTED,
		// not what was thrown at it. A shed request never reached a database
		// connection, so counting it as throughput measures how fast the
		// bulkhead can decline, which is fast, and meaningless.
		offered := float64(opts.orders)
		admitted := offered - float64(shed.Load()) - float64(throttled.Load())
		log.Printf("per-replica HTTP capacity: %.0f admitted request(s)/s at a bulkhead of %d (%.0f offered/s, %d shed with 503, %d throttled with 429)",
			admitted/stormTook.Seconds(), opts.maxInFlight,
			offered/stormTook.Seconds(), shed.Load(), throttled.Load())
		if shedShare := float64(shed.Load()) / offered; shedShare > 0.05 {
			log.Printf("NOTE: %.0f%% of requests were shed, so the offered load was above this replica's capacity, "+
				"the admitted figure is the ceiling, and the edge admission rate belongs below it", shedShare*100)
		}
	} else {
		log.Printf("use-case throughput: %.0f attempt(s)/s; inventory arbitration only, NOT a per-replica HTTP ceiling",
			float64(opts.orders)/stormTook.Seconds())
	}
	log.Printf("checkout latency: p50 %s  p95 %s  p99 %s",
		checkoutLatency.percentile(50).Round(time.Microsecond),
		checkoutLatency.percentile(95).Round(time.Microsecond),
		checkoutLatency.percentile(99).Round(time.Microsecond))

	// --- charges --------------------------------------------------------------
	chargesStarted := time.Now()
	if err := drain(ctx, db, runID, opts.drainTimeout, "charges"); err != nil {
		return err
	}
	log.Printf("provider: %d charge(s) created, %d transient failure(s) retried, in %s",
		provider.created.Load(), provider.failures.Load(), time.Since(chargesStarted).Round(time.Millisecond))

	// --- the provider decides, and tells us, repeatedly ----------------------
	webhooksStarted := time.Now()
	approved, rejected, expired := provider.settleEverything()
	log.Printf("provider: %d approved, %d rejected, %d expired; %d/%d webhook delivery(ies) accepted, %d lost in %s",
		approved, rejected, expired, provider.delivered.Load(), provider.webhookAttempts.Load(), provider.webhookFailures.Load(),
		time.Since(webhooksStarted).Round(time.Millisecond))

	settlementStarted := time.Now()
	if err := drain(ctx, db, runID, opts.drainTimeout, "settlement"); err != nil {
		return err
	}
	log.Printf("settlement: notification-driven syncs drained in %s", time.Since(settlementStarted).Round(time.Millisecond))

	// Webhooks are notifications, never the only route to correctness. The
	// stub deliberately does not retry failed deliveries; schedule recovery for
	// this run's remaining pending orders exactly as the production sweep does.
	recoveryStarted := time.Now()
	recovered, err := scheduleRunReconciliation(ctx, db, runID, payments)
	if err != nil {
		return err
	}
	if recovered > 0 {
		if err := drain(ctx, db, runID, opts.drainTimeout, "reconciliation"); err != nil {
			return err
		}
	}
	log.Printf("reconciliation: %d pending order(s) recovered in %s", recovered, time.Since(recoveryStarted).Round(time.Millisecond))

	stopWorkers()
	workersDone.Wait()

	// --- the audit ------------------------------------------------------------
	return audit(db, runID, tierIDs, provider, int(created.Load()), refused)
}

func newHTTP2Server(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	return server
}

// statementErrors counts every statement the database refused during the run.
//
// Nothing the system does under load should fail at the database. Every
// collision it expects: a webhook delivered three times, a checkout retried
// over a dropped connection, a tier that just sold out; is decided by a
// statement that SUCCEEDS. A count above zero is a mechanism using errors as
// control flow, which at a million events is a rolled-back transaction and a
// line in the database log per event, and that is what this invariant
// forbids.
type statementErrors struct {
	gormlogger.Interface
	count   atomic.Int64
	mu      sync.Mutex
	samples []string
}

func (l *statementErrors) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	// "Not found" is an answer, not a refusal; a cancelled context is the
	// harness shutting down.
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && ctx.Err() == nil {
		l.count.Add(1)
		l.mu.Lock()
		if len(l.samples) < 3 {
			sql, _ := fc()
			if len(sql) > 120 {
				sql = sql[:120] + "…"
			}
			l.samples = append(l.samples, fmt.Sprintf("%v [%s]", err, sql))
		}
		l.mu.Unlock()
	}
	l.Interface.Trace(ctx, begin, fc, err)
}

func (l *statementErrors) report() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.samples, " | ")
}

// ownJobs is the predicate that scopes a jobs query to this run, through the
// orders the jobs name; the table is shared with other runs, the test suite
// and the API, and their jobs are not this run's concern.
//
// Two IN-subqueries rather than one EXISTS with an OR: PostgreSQL evaluates
// each subquery once into a hash and probes it per job, where the OR inside a
// correlated EXISTS forces a scan of the run's orders for every job: quadratic,
// and at twenty thousand orders the harness spent longer counting the queue
// than the queue spent draining.
const ownJobs = `(j.payload->>'orderId' IN (SELECT id FROM orders WHERE idempotency_key LIKE @run)
	OR j.payload->>'paymentId' IN (SELECT payment_id FROM orders WHERE idempotency_key LIKE @run AND payment_id <> ''))`

// outstanding counts the run's own unfinished jobs: running, or pending and
// not a future hold expiry.
func outstanding(ctx context.Context, db *gorm.DB, runID string) int64 {
	var total int64
	db.WithContext(ctx).Raw(`
		SELECT COUNT(*) FROM jobs j
		WHERE (j.status = 'processing'
		   OR (j.status = 'pending' AND NOT (j.type = @expire AND j.run_at > NOW())))
		  AND `+ownJobs,
		map[string]any{"expire": queuedomain.TypeExpireHolds, "run": "load-" + runID + "-%"}).Scan(&total)
	return total
}

// drain waits until none of the run's jobs is pending or running, retries
// included.
func drain(ctx context.Context, db *gorm.DB, runID string, timeout time.Duration, phase string) error {
	deadline := time.Now().Add(timeout)
	for {
		open := outstanding(ctx, db, runID)
		if open == 0 {
			// One extra beat for in-flight handlers to commit.
			time.Sleep(300 * time.Millisecond)
			if outstanding(ctx, db, runID) == 0 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: queue did not drain within %s (%d job(s) still open)", phase, timeout, open)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func scheduleRunReconciliation(
	ctx context.Context,
	db *gorm.DB,
	runID string,
	payments *paymentUsecase.Service,
) (int, error) {
	var ids []string
	if err := db.WithContext(ctx).Raw(`
		SELECT id FROM orders
		WHERE idempotency_key LIKE ? AND status = ?
		ORDER BY updated_at, id`, "load-"+runID+"-%", string(orderdomain.StatusPendingPayment)).Scan(&ids).Error; err != nil {
		return 0, err
	}
	scheduled := 0
	for _, id := range ids {
		added, err := payments.ScheduleReconciliation(ctx, id)
		if err != nil {
			return scheduled, err
		}
		if added {
			scheduled++
		}
	}
	return scheduled, nil
}

type tierRow struct {
	ID       string
	Quantity int
	Sold     int
	Reserved int
}

func audit(db *gorm.DB, runID string, tierIDs []string, provider *stubProvider, opened int, refused *statementErrors) error {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// 1. Stock never exceeded capacity, and the counters match the orders.
	var tiers []tierRow
	if err := db.Raw("SELECT id, quantity, sold, reserved FROM tickets WHERE id IN ?", tierIDs).Scan(&tiers).Error; err != nil {
		return err
	}
	for _, tier := range tiers {
		if tier.Sold+tier.Reserved > tier.Quantity {
			fail("tier %s oversold: sold %d + reserved %d > capacity %d", tier.ID, tier.Sold, tier.Reserved, tier.Quantity)
		}
		var paidQuantity, pendingQuantity int
		// Through the lines, because a tier is no longer a column on the order.
		// This is the audit that would silently keep passing against a stale
		// denormalised column, which is why those columns were dropped rather
		// than left behind.
		const sumHeld = `
			SELECT COALESCE(SUM(i.quantity), 0)
			FROM order_items i
			JOIN orders o ON o.id = i.order_id
			WHERE i.ticket_id = ? AND o.status = ?`
		db.Raw(sumHeld, tier.ID, string(orderdomain.StatusPaid)).Scan(&paidQuantity)
		db.Raw(sumHeld, tier.ID, string(orderdomain.StatusPendingPayment)).Scan(&pendingQuantity)
		if tier.Sold != paidQuantity {
			fail("tier %s: sold counter %d != %d tickets across paid orders", tier.ID, tier.Sold, paidQuantity)
		}
		if tier.Reserved != pendingQuantity {
			fail("tier %s: reserved counter %d != %d tickets across pending orders", tier.ID, tier.Reserved, pendingQuantity)
		}
	}

	// 2. Every order that was opened is still there.
	var stored int64
	db.Raw("SELECT COUNT(*) FROM orders WHERE idempotency_key LIKE ?", "load-"+runID+"-%").Scan(&stored)
	if int(stored) != opened {
		fail("orders lost: %d opened, %d stored", opened, stored)
	}

	// 3. Every approved charge settled exactly one order, and nothing was
	//    charged twice.
	approvedIDs := provider.approvedIDs()
	type paidRow struct {
		PaymentID string
		Status    string
		Count     int
	}
	var byPayment []paidRow
	db.Raw(`SELECT payment_id, status, COUNT(*) AS count FROM orders
	        WHERE idempotency_key LIKE ? AND payment_id <> '' GROUP BY payment_id, status`, "load-"+runID+"-%").Scan(&byPayment)
	seenPayments := map[string]int{}
	for _, row := range byPayment {
		seenPayments[row.PaymentID] += row.Count
		if approvedIDs[row.PaymentID] && row.Status != string(orderdomain.StatusPaid) && row.Status != string(orderdomain.StatusRefundRequired) {
			fail("payment %s was approved but its order is %s", row.PaymentID, row.Status)
		}
		if !approvedIDs[row.PaymentID] && row.Status == string(orderdomain.StatusPaid) {
			fail("payment %s was never approved but its order is paid", row.PaymentID)
		}
	}
	for paymentID, count := range seenPayments {
		if count != 1 {
			fail("payment %s is attached to %d orders", paymentID, count)
		}
	}
	for paymentID := range approvedIDs {
		if seenPayments[paymentID] == 0 {
			fail("approved payment %s belongs to no order: money taken, nothing sold", paymentID)
		}
	}
	var charged int64
	db.Raw("SELECT COUNT(*) FROM orders WHERE idempotency_key LIKE ? AND payment_id <> ''", "load-"+runID+"-%").Scan(&charged)
	if provider.created.Load() != charged {
		fail("provider created %d charges for %d charged orders", provider.created.Load(), charged)
	}

	// 4. Nothing of this run was left behind in the queue.
	var dead int64
	db.Raw(`SELECT COUNT(*) FROM jobs j WHERE j.status = 'dead' AND `+ownJobs,
		map[string]any{"run": "load-" + runID + "-%"}).Scan(&dead)
	if dead > 0 {
		fail("%d job(s) parked in the dead-letter state", dead)
	}

	// 5. The database refused nothing. Duplicate webhooks, retried keys and
	//    sold-out tiers were all decided by statements that succeeded.
	if count := refused.count.Load(); count > 0 {
		fail("%d statement(s) failed at the database, e.g. %s", count, refused.report())
	}

	// --- summary --------------------------------------------------------------
	var summary []struct {
		Status string
		Count  int
	}
	db.Raw("SELECT status, COUNT(*) AS count FROM orders WHERE idempotency_key LIKE ? GROUP BY status ORDER BY status", "load-"+runID+"-%").Scan(&summary)
	for _, row := range summary {
		log.Printf("orders %-16s %d", row.Status, row.Count)
	}
	var totalSold, totalCapacity int
	for _, tier := range tiers {
		totalSold += tier.Sold
		totalCapacity += tier.Quantity
	}
	log.Printf("stock: %d of %d tickets sold across %d tier(s)", totalSold, totalCapacity, len(tiers))

	if len(problems) > 0 {
		for _, problem := range problems {
			log.Printf("INVARIANT VIOLATED: %s", problem)
		}
		return fmt.Errorf("%d invariant(s) violated", len(problems))
	}
	log.Printf("database: %d statement(s) refused", refused.count.Load())
	log.Printf("ALL INVARIANTS HOLD: no oversell, no lost order, no double charge, every approved payment settled, queue drained, nothing refused by the database")
	return nil
}

// cleanup removes the run's rows; jobs first, because a job whose order is
// gone is an orphan the next run would otherwise have to park.
func cleanup(db *gorm.DB, runID, ownerID string) {
	db.Exec(`DELETE FROM jobs j WHERE `+ownJobs, map[string]any{"run": "load-" + runID + "-%"})
	db.Exec("DELETE FROM orders WHERE idempotency_key LIKE ?", "load-"+runID+"-%")
	db.Exec("DELETE FROM tickets WHERE id LIKE ?", "tkt_load_"+runID+"_%")
	db.Exec("DELETE FROM events WHERE id = ?", "evt_load_"+runID)
	db.Exec("DELETE FROM users WHERE id = ?", ownerID)
}

func randomID() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}
