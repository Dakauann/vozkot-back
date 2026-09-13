package container

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	delivery "vozkot/delivery/http"
	authHTTP "vozkot/delivery/http/auth"
	checkoutHTTP "vozkot/delivery/http/checkout"
	eventHTTP "vozkot/delivery/http/event"
	ticketHTTP "vozkot/delivery/http/ticket"
	webhooksHTTP "vozkot/delivery/http/webhooks"
	authdomain "vozkot/domain/auth"
	cachedomain "vozkot/domain/cache"
	idempotencydomain "vozkot/domain/idempotency"
	notificationdomain "vozkot/domain/notification"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/config"
	"vozkot/infra/crypto/pii"
	"vozkot/infra/crypto/piigorm"
	"vozkot/infra/database"
	"vozkot/infra/geocoding"
	authMiddleware "vozkot/infra/http/middleware"
	"vozkot/infra/imaging"
	"vozkot/infra/mercadopago"
	"vozkot/infra/notifications"
	"vozkot/infra/rabbitmq"
	redisCache "vozkot/infra/redis"
	authRepository "vozkot/infra/repositories/auth"
	eventRepository "vozkot/infra/repositories/event"
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
	eventUsecase "vozkot/usecases/event"
	mediaUsecase "vozkot/usecases/media"
	notificationUsecase "vozkot/usecases/notification"
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
	// challenges sweeps spent sign-in codes. Nil when passwordless sign-in is
	// not configured, and the sweep is simply not scheduled.
	challenges *authUsecase.Verification
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

	mediaLibrary := mediaUsecase.NewService(mediaRepository.NewMediaRepository(db), fileStorage, imaging.NewLibrary(cfg.MediaProcessors))

	// Reads go through the cache; the transactional writes inside a unit of
	// work use the undecorated repositories the runner builds.
	var tickets ticketdomain.Repository = ticketRepository.NewTicketRepository(db)
	if readCache != nil {
		tickets = ticketRepository.NewCachedTicketRepository(tickets, readCache, ticketRepository.TicketTTL)
	}
	ticketService := ticketUsecase.NewService(tickets)
	ticketHandler := ticketHTTP.NewHandler(ticketService)

	// The catalogue. Artwork hangs off the event because that is what it
	// depicts, and geocoding is best-effort: no key, no account, and an address
	// nobody can place still publishes.
	events := eventRepository.NewEventRepository(db)
	eventService := eventUsecase.NewService(events, tickets, mediaLibrary, geocoding.New())
	eventHandler := eventHTTP.NewHandler(eventService)

	users := userRepository.NewUserRepository(db)
	// One decorator instance serves both the use case and the guard, which is
	// what makes a logout take effect immediately: Logout revokes through the
	// same object that caches the liveness the guard reads.
	var sessions authdomain.SessionRepository = authRepository.NewSessionRepository(db)
	if readCache != nil && cfg.Cache.SessionCacheTTL > 0 {
		sessions = authRepository.NewCachedSessionRepository(sessions, readCache, cfg.Cache.SessionCacheTTL)
		log.Printf("auth: session liveness cached for %s (the busiest query in the system)", cfg.Cache.SessionCacheTTL)
	}

	// One policy about which forwarding headers may be believed, applied
	// everywhere a client address is used.
	clients, err := authMiddleware.NewClientIP(cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}

	var loginLimit *authMiddleware.RateLimit
	if limiter != nil {
		loginLimit = authMiddleware.NewRateLimit(limiter, "login", cfg.Cache.LoginEmailRateLimit, cfg.Cache.AuthRateWindow, clients)
	}

	// The encryption keyring, installed before any repository that touches a
	// sealed column. Optional: a clone with no keys runs with the passwordless
	// routes unmounted rather than writing documents in the clear.
	piiService, err := pii.LoadFromEnv()
	if err != nil {
		if os.Getenv("APP_ENV") == "production" {
			// Not a preference. Holding documents, legal names and dates of
			// birth without keys is the shape of a breach, and a box office
			// that cannot encrypt them must not be the one taking them.
			return nil, fmt.Errorf("PII encryption keys are required in production: %w", err)
		}
		log.Printf("auth: PII encryption is not configured (%v); passwordless sign-in is disabled", err)
	} else {
		piigorm.SetService(piiService)
		log.Printf("auth: PII encryption active, key version %d", piiService.ActiveKEKVersion())
	}

	authService := authUsecase.NewService(users, sessions, passwords, tokens, cfg.RefreshTokenTTL)
	authHandler := authHTTP.NewHandler(authService, authHTTP.CookieConfig{
		Domain: cfg.CookieDomain, Secure: cfg.CookieSecure,
		AccessMaxAge: cfg.AccessTokenTTL, RefreshMaxAge: cfg.RefreshTokenTTL,
	}, loginLimit, clients.From)
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
	holdLimits := orderdomain.HoldLimits{
		Orders:         cfg.CheckoutMaxOpenOrders,
		TicketsPerTier: cfg.CheckoutMaxHeldPerTier,
	}
	if !holdLimits.Unlimited() {
		log.Printf("checkout: one account may hold %d open order(s) and %d ticket(s) per tier",
			holdLimits.Orders, holdLimits.TicketsPerTier)
	}
	// Buyer messaging. Built before the payment service because settlement is
	// what raises a receipt, and built as ONE stack — renderer, channel
	// senders, notifier — so the channels the notifier will accept are exactly
	// the channels something can deliver.
	messenger, purchases, notifier, err := buildNotifications(cfg.Notifications, jobs, dispatcher)
	if err != nil {
		return nil, err
	}

	// Passwordless sign-in, and the identity block it later asks for.
	//
	// Both depend on the encryption keyring: the challenge table seals its
	// destinations and the user table seals its documents, so without keys
	// there is nowhere safe to put either. Rather than fall back to plaintext —
	// which is how a "temporary" unencrypted column becomes permanent — the
	// routes are simply not mounted, and the password sign-in that predates
	// them keeps working.
	if piiService != nil {
		codes := notificationUsecase.NewCodeSender(notifier, dispatcher)
		challenges := authRepository.NewChallengeRepository(db)
		verification := authUsecase.NewVerification(
			users, challenges, passwords,
			piiService.BlindIndex,
			codes, notificationUsecase.NewMockPhoneSender(),
			authService,
		)
		authHandler = authHandler.WithVerification(verification, authUsecase.NewProfiles(users))
		container.challenges = verification
		log.Printf("auth: passwordless sign-in enabled; codes are %d digits and last %s",
			authdomain.CodeLength, authdomain.CodeTTL)
	}

	checkoutService := checkoutUsecase.NewService(unit, orders, tickets, dispatcher, cfg.Payments.HoldFor, cfg.Payments.CartHoldFor, holdLimits)
	paymentService := paymentUsecase.NewService(unit, orders, gateway, jobs, dispatcher, purchases)
	checkoutHandler := checkoutHTTP.NewHandler(checkoutService, paymentService, eventService, keys, cfg.Queue.IdempotencyLease)

	var webhookHandler *webhooksHTTP.MercadoPagoHandler
	if cfg.Payments.Enabled() {
		webhookHandler = webhooksHTTP.NewMercadoPagoHandler(jobs, dispatcher, cfg.Payments.WebhookSecret, cfg.Payments.SignatureTolerance)
		log.Printf("payments: Mercado Pago enabled, holds last %s", cfg.Payments.HoldFor)
	} else {
		log.Printf("payments: Mercado Pago is not configured; checkout will answer 503 until MERCADOPAGO_ACCESS_TOKEN is set")
	}

	var checkoutLimit, authLimit *authMiddleware.RateLimit
	if limiter != nil {
		checkoutLimit = authMiddleware.NewRateLimit(limiter, "checkout", cfg.Cache.CheckoutRateLimit, cfg.Cache.CheckoutRateWindow, clients)
		authLimit = authMiddleware.NewRateLimit(limiter, "auth", cfg.Cache.AuthRateLimit, cfg.Cache.AuthRateWindow, clients)
	}
	checkoutAdmission := authMiddleware.NewConcurrencyLimit(cfg.CheckoutMaxInFlight)

	router := delivery.NewRouter(delivery.Dependencies{
		Auth:              authHandler,
		Tickets:           ticketHandler,
		Events:            eventHandler,
		Checkout:          checkoutHandler,
		Webhooks:          webhookHandler,
		AuthMiddleware:    authGuard,
		CheckoutLimit:     checkoutLimit,
		CheckoutAdmission: checkoutAdmission,
		AuthLimit:         authLimit,
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

	container.startWorkers(cfg, jobs, keys, consumer, dispatcher, checkoutService, paymentService, messenger)
	return container, nil
}

// buildNotifications assembles the messaging stack, or reports that there is
// none.
//
// Everything is decided here and nowhere else: which provider carries email,
// which channels therefore exist, and what the notifier is allowed to queue.
// With no RESEND_API_KEY it returns nils all the way down — a supported state
// in development, and one config.loadNotifications refuses in production,
// because a box office that takes money and tells nobody is worse than one
// that will not start.
func buildNotifications(
	cfg config.NotificationsConfig,
	jobs queuedomain.Queue,
	dispatcher *queueUsecase.Dispatcher,
) (*notificationUsecase.Service, *notificationUsecase.Purchases, *notificationUsecase.Notifier, error) {
	email := notifications.NewEmailSender(cfg)
	if email == nil {
		log.Printf("notifications: RESEND_API_KEY is not set; buyers receive no order emails")
		return nil, nil, nil, nil
	}

	// Parsed once, at boot. A template with a syntax error or a missing
	// component fails the deploy here rather than one buyer's receipt later.
	renderer, err := notifications.NewRenderer(cfg.Brand)
	if err != nil {
		return nil, nil, nil, err
	}

	messenger := notificationUsecase.NewService(renderer, email)
	notifier := notificationUsecase.NewNotifier(jobs, dispatcher, messenger.Channels()...)
	purchases := notificationUsecase.NewPurchases(notifier, cfg.Brand.SiteURL)

	log.Printf("notifications: Resend enabled, %d template(s), from %q, up to %d/s per replica",
		renderer.Templates(), cfg.FromEmail, cfg.MaxRPS)
	return messenger, purchases, notifier, nil
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
	messenger *notificationUsecase.Service,
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
			// Spent sign-in codes ride the same sweep rather than getting a
			// timer of their own. They expire on the same order of minutes as a
			// hold, and keeping them afterwards would be retaining a record of
			// who signed in and when — which is exactly what that table's shape
			// avoids holding.
			if c.challenges != nil {
				if _, sweepErr := c.challenges.SweepExpiredChallenges(ctx, 500); sweepErr != nil {
					log.Printf("auth: sweeping expired challenges: %v", sweepErr)
				}
			}
			return err
		})

		// Registered only when a provider exists. A worker with no handler for
		// a type PARKS those jobs, and the notifier writes none in that state,
		// so the two agree: with messaging off, nothing is queued and nothing
		// is parked.
		if messenger != nil {
			worker.Handle(queuedomain.TypeSendNotification, func(ctx context.Context, job queuedomain.Job) error {
				payload, err := queueUsecase.Decode[notificationdomain.Payload](job)
				if err != nil {
					return err
				}
				return messenger.Deliver(ctx, payload)
			})
		}

		worker.Handle(queuedomain.TypeReconcile, func(ctx context.Context, _ queuedomain.Job) error {
			scheduled, err := payments.Reconcile(ctx, cfg.Queue.ReconcileBatchSize)
			if err == nil && scheduled > 0 {
				log.Printf("queue: re-checking %d pending payment(s)", scheduled)
			}
			return err
		})

		// The hourly counterpart: orders that already settled. It is the only
		// thing that notices a refund or chargeback whose notification never
		// arrived, which would otherwise leave money returned and the seat
		// still counted as sold.
		worker.Handle(queuedomain.TypeAuditSettled, func(ctx context.Context, _ queuedomain.Job) error {
			scheduled, err := payments.AuditSettled(ctx, cfg.Queue.AuditBatchSize)
			if err == nil && scheduled > 0 {
				log.Printf("queue: auditing %d settled payment(s)", scheduled)
			}
			return err
		})

		worker.Handle(queuedomain.TypeRefundCharge, func(ctx context.Context, job queuedomain.Job) error {
			payload, err := queueUsecase.Decode[queuedomain.SyncPaymentPayload](job)
			if err != nil {
				return err
			}
			return payments.Refund(ctx, payload.OrderID)
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
