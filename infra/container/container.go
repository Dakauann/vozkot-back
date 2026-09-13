package container

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	delivery "vozkot/delivery/http"
	authHTTP "vozkot/delivery/http/auth"
	checkoutHTTP "vozkot/delivery/http/checkout"
	ticketHTTP "vozkot/delivery/http/ticket"
	webhooksHTTP "vozkot/delivery/http/webhooks"
	cachedomain "vozkot/domain/cache"
	idempotencydomain "vozkot/domain/idempotency"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/config"
	"vozkot/infra/database"
	authMiddleware "vozkot/infra/http/middleware"
	"vozkot/infra/mercadopago"
	"vozkot/infra/rabbitmq"
	redisCache "vozkot/infra/redis"
	authRepository "vozkot/infra/repositories/auth"
	idempotencyRepository "vozkot/infra/repositories/idempotency"
	mediaRepository "vozkot/infra/repositories/media"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/security"
	"vozkot/infra/storage"
	"vozkot/infra/uow"
	authUsecase "vozkot/usecases/auth"
	checkoutUsecase "vozkot/usecases/checkout"
	mediaUsecase "vozkot/usecases/media"
	paymentUsecase "vozkot/usecases/payment"
	queueUsecase "vozkot/usecases/queue"
	ticketUsecase "vozkot/usecases/ticket"
)

type Container struct {
	server   *http.Server
	database *sql.DB
	// broker and cache are closed on shutdown; both are nil when not configured.
	broker *rabbitmq.Broker
	cache  *redisCache.Cache
	// workers is cancelled on shutdown so in-flight jobs finish before the
	// process exits rather than being reclaimed minutes later by another node.
	stopWorkers context.CancelFunc
	workersDone sync.WaitGroup
}

func New(cfg config.Config) (*Container, error) {
	passwords := security.NewPasswordService(security.MinPasswordHashCost)
	tokens, err := security.NewTokenService(cfg.JWTSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	if err != nil {
		return nil, err
	}

	startupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationDB, err := database.NewMigrationDatabase(startupContext, cfg.Database)
	if err != nil {
		return nil, err
	}
	migrationSQL, err := migrationDB.DB()
	if err != nil {
		return nil, err
	}
	defer migrationSQL.Close()
	if err := database.RunMigrations(startupContext, migrationDB); err != nil {
		return nil, err
	}

	db, err := database.NewApplicationDatabase(startupContext, cfg.Database)
	if err != nil {
		return nil, err
	}
	databaseSQL, err := db.DB()
	if err != nil {
		return nil, err
	}

	// The object store is chosen once, here. Everything above infra sees the
	// FileStorage port and cannot tell Cloudflare R2 from a local directory.
	fileStorage, err := storage.New(startupContext, cfg.Media)
	if err != nil {
		return nil, err
	}
	var mediaFiles http.Handler
	if local, ok := fileStorage.(*storage.Local); ok {
		mediaFiles = local.FileHandler()
		log.Printf("media storage: local directory %q (set the CLOUDFLARE_R2_* variables to use Cloudflare R2)", cfg.Media.LocalDir)
	} else {
		log.Printf("media storage: Cloudflare R2 bucket %q", cfg.Media.Bucket)
	}

	container := &Container{database: databaseSQL}

	// Redis: read cache and rate limiter. Both are load shedding, so a missing
	// Redis degrades performance and nothing else.
	var readCache cachedomain.Cache
	var limiter cachedomain.RateLimiter
	if cfg.Cache.Enabled() {
		connected, err := redisCache.Connect(startupContext, cfg.Cache.URL, cfg.Cache.KeyPrefix)
		if err != nil {
			return nil, err
		}
		container.cache = connected
		readCache = connected
		limiter = redisCache.NewRateLimiter(connected)
		log.Printf("cache: Redis connected, key prefix %q", cfg.Cache.KeyPrefix)
	} else {
		log.Printf("cache: REDIS_URL is not set; reads go straight to PostgreSQL and the checkout rate limit is off")
	}

	mediaLibrary := mediaUsecase.NewService(mediaRepository.NewMediaRepository(db), fileStorage)

	// Reads go through the cache; the transactional writes inside a unit of
	// work use the undecorated repositories the runner builds.
	var tickets ticketdomain.Repository = ticketRepository.NewTicketRepository(db)
	if readCache != nil {
		tickets = ticketRepository.NewCachedTicketRepository(tickets, readCache, ticketRepository.TicketTTL)
	}
	ticketService := ticketUsecase.NewService(tickets, mediaLibrary)
	ticketHandler := ticketHTTP.NewHandler(ticketService)

	users := userRepository.NewUserRepository(db)
	sessions := authRepository.NewSessionRepository(db)
	authService := authUsecase.NewService(users, sessions, passwords, tokens, cfg.RefreshTokenTTL)
	authHandler := authHTTP.NewHandler(authService, authHTTP.CookieConfig{
		Domain: cfg.CookieDomain, Secure: cfg.CookieSecure,
		AccessMaxAge: cfg.AccessTokenTTL, RefreshMaxAge: cfg.RefreshTokenTTL,
	})
	authGuard := authMiddleware.NewAuth(tokens, sessions)

	// Purchases: the unit of work binds inventory, orders and the job queue to
	// one transaction, which is what keeps stock and money from disagreeing.
	unit := uow.NewRunner(db)
	var orders orderdomain.Repository = orderRepository.NewOrderRepository(db)
	if readCache != nil {
		orders = orderRepository.NewCachedOrderRepository(orders, readCache, orderRepository.OrderTTL)
	}
	jobs := queueRepository.NewJobRepository(db)
	keys := idempotencyRepository.NewIdempotencyRepository(db)

	// RabbitMQ: the transport. The job table stays the ledger, so a broker that
	// is down slows dispatch to the poller cadence instead of stopping work.
	var publisher queuedomain.Publisher
	var consumer queuedomain.Consumer
	if cfg.Broker.Enabled() {
		broker, err := rabbitmq.Connect(cfg.Broker.URL, rabbitmq.WithPrefetch(cfg.Broker.Prefetch),
			rabbitmq.WithQueueType(cfg.Broker.QueueType))
		if err != nil {
			return nil, err
		}
		container.broker = broker
		publisher = broker
		consumer = broker
		log.Printf("queue: RabbitMQ connected, %s queue, prefetch %d", cfg.Broker.QueueType, cfg.Broker.Prefetch)
	} else {
		log.Printf("queue: RABBITMQ_URL is not set; jobs are dispatched by the database poller alone")
	}
	dispatcher := queueUsecase.NewDispatcher(publisher)

	gateway := buildGateway(cfg.Payments)
	checkoutService := checkoutUsecase.NewService(unit, orders, tickets, dispatcher, cfg.Payments.HoldFor)
	paymentService := paymentUsecase.NewService(unit, orders, gateway, jobs, dispatcher)
	checkoutHandler := checkoutHTTP.NewHandler(checkoutService, paymentService, keys)

	var webhookHandler *webhooksHTTP.MercadoPagoHandler
	if cfg.Payments.Enabled() {
		webhookHandler = webhooksHTTP.NewMercadoPagoHandler(jobs, dispatcher, cfg.Payments.WebhookSecret, cfg.Payments.SignatureTolerance)
		log.Printf("payments: Mercado Pago enabled, holds last %s", cfg.Payments.HoldFor)
	} else {
		log.Printf("payments: Mercado Pago is not configured; checkout will answer 503 until MERCADOPAGO_ACCESS_TOKEN is set")
	}

	var checkoutLimit *authMiddleware.RateLimit
	if limiter != nil {
		checkoutLimit = authMiddleware.NewRateLimit(limiter, "checkout", cfg.Cache.CheckoutRateLimit, cfg.Cache.CheckoutRateWindow)
	}
	checkoutAdmission := authMiddleware.NewConcurrencyLimit(cfg.CheckoutMaxInFlight)

	router := delivery.NewRouter(delivery.Dependencies{
		Auth:              authHandler,
		Tickets:           ticketHandler,
		Checkout:          checkoutHandler,
		Webhooks:          webhookHandler,
		AuthMiddleware:    authGuard,
		CheckoutLimit:     checkoutLimit,
		CheckoutAdmission: checkoutAdmission,
		MediaFiles:        mediaFiles,
		AllowedOrigin:     cfg.CORSAllowedOrigin,
		Health:            queueHealth(jobs, container),
	})

	container.server = &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	container.startWorkers(cfg, jobs, keys, consumer, dispatcher, checkoutService, paymentService)
	return container, nil
}

// buildGateway returns the payment adapter, or nil when the provider is not
// configured. A nil gateway is a supported state: the box office still manages
// tickets, and checkout answers "payment provider is not configured".
func buildGateway(cfg config.PaymentsConfig) paymentdomain.Gateway {
	if !cfg.Enabled() {
		return nil
	}
	options := []mercadopago.Option{}
	if cfg.NotificationURL != "" {
		options = append(options, mercadopago.WithNotificationURL(cfg.NotificationURL))
	}
	client := mercadopago.NewClient(cfg.AccessToken, cfg.BaseURL, options...)

	gatewayOptions := []mercadopago.GatewayOption{}
	if cfg.SandboxPayerEmail != "" {
		gatewayOptions = append(gatewayOptions, mercadopago.WithSandboxPayerEmail(cfg.SandboxPayerEmail))
	}
	return mercadopago.NewGateway(client, gatewayOptions...)
}

// startWorkers runs the queue in this process.
//
// In-process by default because it is one binary to deploy and the work is
// small; the same worker runs standalone by starting the binary with the HTTP
// server disabled, and nothing in the design assumes a single node — claiming
// is exclusive at the database.
func (c *Container) startWorkers(
	cfg config.Config,
	jobs queuedomain.Queue,
	keys idempotencydomain.Store,
	consumer queuedomain.Consumer,
	dispatcher *queueUsecase.Dispatcher,
	checkout *checkoutUsecase.Service,
	payments *paymentUsecase.Service,
) {
	ctx, cancel := context.WithCancel(context.Background())
	c.stopWorkers = cancel

	for index := 0; index < cfg.Queue.Workers; index++ {
		options := []queueUsecase.Option{
			queueUsecase.WithBatchSize(cfg.Queue.BatchSize),
			queueUsecase.WithInterval(cfg.Queue.PollInterval),
			queueUsecase.WithJobTimeout(cfg.Queue.JobTimeout),
		}
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
			// A job carrying an order id is that order's own expiry; one
			// without is the periodic sweep.
			if payload.OrderID != "" {
				return checkout.ExpireHold(ctx, payload.OrderID)
			}
			released, err := checkout.ExpireHolds(ctx, 200)
			if err == nil && released > 0 {
				log.Printf("queue: released %d expired hold(s)", released)
			}
			return err
		})

		worker.Handle(queuedomain.TypeReconcile, func(ctx context.Context, _ queuedomain.Job) error {
			scheduled, err := payments.Reconcile(ctx, cfg.Queue.ReconcileBatchSize)
			if err == nil && scheduled > 0 {
				log.Printf("queue: re-checking %d pending payment(s)", scheduled)
			}
			return err
		})

		worker.Handle(queuedomain.TypeCleanup, func(ctx context.Context, _ queuedomain.Job) error {
			pruner, ok := jobs.(queuedomain.CompletedPruner)
			if !ok {
				return nil
			}
			deletedJobs, err := pruner.DeleteCompletedBefore(ctx,
				time.Now().UTC().Add(-cfg.Queue.CompletedRetention), cfg.Queue.CleanupBatchSize)
			if err != nil {
				return err
			}
			deletedKeys, err := keys.DeleteExpired(ctx, time.Now().UTC())
			if err != nil {
				return err
			}
			if deletedJobs > 0 || deletedKeys > 0 {
				log.Printf("queue: retention removed %d completed job(s) and %d expired idempotency key(s)",
					deletedJobs, deletedKeys)
			}
			return nil
		})

		c.workersDone.Add(1)
		go func() {
			defer c.workersDone.Done()
			worker.Run(ctx)
		}()
	}

	scheduler := queueUsecase.NewScheduler(jobs, dispatcher, cfg.Queue.SweepInterval)
	c.workersDone.Add(1)
	go func() {
		defer c.workersDone.Done()
		scheduler.Run(ctx)
	}()
}

// queueHealth exposes the queue depth, so "payments are stuck" is visible from
// a health check instead of from a customer complaint.
func queueHealth(jobs queuedomain.Queue, container *Container) func() map[string]any {
	return func() map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		pending, err := jobs.CountByStatus(ctx, queuedomain.StatusPending)
		if err != nil {
			return map[string]any{"queue": "unavailable"}
		}
		dead, err := jobs.CountByStatus(ctx, queuedomain.StatusDead)
		if err != nil {
			return map[string]any{"queue": map[string]any{"pending": pending}}
		}

		health := map[string]any{"pending": pending, "dead": dead}
		if container != nil && container.broker != nil {
			if depth, err := container.broker.QueueDepth(); err == nil {
				health["broker"] = depth
			} else {
				health["broker"] = "unavailable"
			}
		}
		return map[string]any{"queue": health}
	}
}

func (c *Container) Start() error {
	return c.server.ListenAndServe()
}

func (c *Container) Shutdown(ctx context.Context) error {
	serverErr := c.server.Shutdown(ctx)
	if c.stopWorkers != nil {
		c.stopWorkers()
	}
	// Cancelling stops the workers CLAIMING; the jobs already in hand run to
	// their end and record themselves, and c.workersDone is what waits for
	// them. A job abandoned mid-charge would otherwise sit marked processing
	// until the stale sweep, and then run a second time.
	done := make(chan struct{})
	go func() {
		c.workersDone.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	var brokerErr, cacheErr error
	if c.broker != nil {
		brokerErr = c.broker.Close()
	}
	if c.cache != nil {
		cacheErr = c.cache.Close()
	}
	databaseErr := c.database.Close()
	return errors.Join(serverErr, brokerErr, cacheErr, databaseErr)
}
