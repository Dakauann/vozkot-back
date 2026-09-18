package container

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	delivery "vozkot/delivery/http"
	admissionHTTP "vozkot/delivery/http/admission"
	authHTTP "vozkot/delivery/http/auth"
	checkoutHTTP "vozkot/delivery/http/checkout"
	eventHTTP "vozkot/delivery/http/event"
	payoutHTTP "vozkot/delivery/http/payout"
	refundHTTP "vozkot/delivery/http/refund"
	reportHTTP "vozkot/delivery/http/report"
	seatingHTTP "vozkot/delivery/http/seating"
	ticketHTTP "vozkot/delivery/http/ticket"
	webhooksHTTP "vozkot/delivery/http/webhooks"
	authdomain "vozkot/domain/auth"
	cachedomain "vozkot/domain/cache"
	idempotencydomain "vozkot/domain/idempotency"
	ledgerdomain "vozkot/domain/ledger"
	notificationdomain "vozkot/domain/notification"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/asaas"
	"vozkot/infra/config"
	"vozkot/infra/crypto/pii"
	"vozkot/infra/crypto/piigorm"
	"vozkot/infra/database"
	"vozkot/infra/geocoding"
	authMiddleware "vozkot/infra/http/middleware"
	"vozkot/infra/imaging"
	"vozkot/infra/mercadopago"
	"vozkot/infra/notifications"
	prometheusMetrics "vozkot/infra/prometheus"
	"vozkot/infra/qrcode"
	"vozkot/infra/rabbitmq"
	redisCache "vozkot/infra/redis"
	admissionRepository "vozkot/infra/repositories/admission"
	authRepository "vozkot/infra/repositories/auth"
	eventRepository "vozkot/infra/repositories/event"
	idempotencyRepository "vozkot/infra/repositories/idempotency"
	mediaRepository "vozkot/infra/repositories/media"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	refundRepository "vozkot/infra/repositories/refund"
	reportRepository "vozkot/infra/repositories/report"
	seatingRepository "vozkot/infra/repositories/seating"
	ticketRepository "vozkot/infra/repositories/ticket"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/security"
	"vozkot/infra/storage"
	"vozkot/infra/uow"
	admissionUsecase "vozkot/usecases/admission"
	authUsecase "vozkot/usecases/auth"
	checkoutUsecase "vozkot/usecases/checkout"
	eventUsecase "vozkot/usecases/event"
	mediaUsecase "vozkot/usecases/media"
	notificationUsecase "vozkot/usecases/notification"
	paymentUsecase "vozkot/usecases/payment"
	payoutUsecase "vozkot/usecases/payout"
	queueUsecase "vozkot/usecases/queue"
	refundUsecase "vozkot/usecases/refund"
	reportUsecase "vozkot/usecases/report"
	seatingUsecase "vozkot/usecases/seating"
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
	// recorder is what the workers and sweeps report to. Never nil; a Service
	// with no listener still records, it is simply never scraped.
	recorder *prometheusMetrics.Service
	// metrics serves /metrics on its own listener. Nil when METRICS_LISTEN_ADDR
	// is empty, and then nothing is exposed and nothing is scraped.
	metrics *metricsServer
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

	// Redis: read cache and rate limiter. The cache half is load shedding; the
	// limiter half is a security control, which is why production refuses to
	// start without a REDIS_URL and only development reaches the else branch.
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
		// Development only: config.Load refuses to build a production
		// configuration without a REDIS_URL. Worth naming the missing CONTROLS
		// and not just the missing cache, so nobody reads this line as
		// "reads will be a bit slower" while the credential routes sit
		// unthrottled.
		log.Printf("cache: REDIS_URL is not set; reads go straight to PostgreSQL and " +
			"the auth, login and checkout rate limits are OFF (development only)")
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
	eventHandler := eventHTTP.NewHandler(eventService, cfg.ServiceFee)

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
	// The door's store and the QR encoder, built here rather than further down
	// because the receipt needs them: a confirmation carries one QR and one
	// printed code per admission, and the source is handed to the messenger
	// below.
	admissions := admissionRepository.NewAdmissionRepository(db)
	admissionTickets := notificationUsecase.NewAdmissionTickets(admissions, qrcode.NewRenderer())

	// Buyer messaging. Built before the payment service because settlement is
	// what raises a receipt, and built as ONE stack: renderer, channel
	// senders, notifier, so the channels the notifier will accept are exactly
	// the channels something can deliver.
	messenger, purchases, notifier, err := buildNotifications(cfg.Notifications, jobs, dispatcher, admissionTickets)
	if err != nil {
		return nil, err
	}

	// Passwordless sign-in, and the identity block it later asks for.
	//
	// Both depend on the encryption keyring: the challenge table seals its
	// destinations and the user table seals its documents, so without keys
	// there is nowhere safe to put either. Rather than fall back to plaintext,
	// which is how a "temporary" unencrypted column becomes permanent, the
	// routes are simply not mounted, and the password sign-in that predates
	// them keeps working.
	if piiService != nil {
		codes := notificationUsecase.NewCodeSender(notifier, dispatcher)
		challenges := authRepository.NewChallengeRepository(db)
		// The phone sender, when a channel that reaches one is actually wired.
		//
		// A nil INTERFACE otherwise, never a typed nil: the use case checks
		// `sender == nil` before it generates a code or writes a challenge row, so
		// nil is what makes the phone routes answer 503 instead of creating a
		// challenge nobody can answer. That is still the honest representation of
		// "this channel does not exist yet"; the alternative once considered here
		// was a stub that logged the code, which puts a live credential in whatever
		// collects stdout.
		var phoneCodes authUsecase.Sender
		if notifier.Handles(notificationdomain.ChannelWhatsApp) {
			phoneCodes = notificationUsecase.NewPhoneCodeSender(notifier, dispatcher, notificationdomain.ChannelWhatsApp)
		}
		verification := authUsecase.NewVerification(
			users, challenges, passwords,
			piiService.BlindIndex,
			codes, phoneCodes,
			authService,
		)
		authHandler = authHandler.WithVerification(verification, authUsecase.NewProfiles(users))
		container.challenges = verification
		log.Printf("auth: passwordless sign-in enabled; codes are %d digits and last %s",
			authdomain.CodeLength, authdomain.CodeTTL)
		// Asked per CHANNEL, not "is there a notifier at all".
		//
		// It used to read `notifier == nil`, which was the same question while email
		// was the only channel. Once a phone channel could exist on its own, that
		// test started passing on a deployment with WhatsApp and no mail provider,
		// so the one warning that explains a 503 on the sign-in screen went silent
		// exactly when it was still true. Handles is nil-safe, so this also covers
		// "no notifier at all".
		if !notifier.Handles(notificationdomain.ChannelEmail) {
			// Said plainly at boot, because the symptom otherwise is a sign-in
			// screen that answers 503 and nobody knowing why.
			log.Printf("auth: WARNING no mail provider; sign-in codes cannot be sent and /auth/email/start will answer 503. Set RESEND_API_KEY and RESEND_FROM_EMAIL.")
		}
		if phoneCodes == nil {
			log.Printf("auth: no phone channel configured; phone confirmation answers 503. Set the VOZKO_* block to deliver codes over WhatsApp.")
		} else {
			log.Printf("auth: phone confirmation delivers codes over %s", notificationdomain.ChannelWhatsApp)
		}
	}

	checkoutService := checkoutUsecase.NewService(unit, orders, tickets, users, dispatcher, checkoutUsecase.Settings{
		HoldFor:     cfg.Payments.HoldFor,
		CartHoldFor: cfg.Payments.CartHoldFor,
		HoldLimits:  holdLimits,
		Fee:         cfg.ServiceFee,
	})
	// The organiser's balance. Standard terms for everyone today; the tier is
	// a lookup the moment there is anything to look up. Weekends only until a
	// holiday calendar is wired, which makes a settlement date land at worst a
	// day early on a feriado, visible, and never money moving that should not.
	payoutService := payoutUsecase.NewService(
		unit, payoutUsecase.AlwaysStandard, ledgerdomain.BankingCalendar, time.Now, prefixedID,
	)
	paymentService := paymentUsecase.NewService(unit, orders, gateway, jobs, dispatcher, purchases).
		WithLedger(payoutService).
		WithReconcileAfter(cfg.Queue.ReconcileAfter)

	// Refunds and reporting hang off the sale rather than being part of it: both
	// read what checkout wrote, and neither can move stock. The refund service
	// borrows the payment service's refund enqueue so an approval and the money
	// it authorises commit together.
	refundRequests := refundRepository.NewRefundRepository(db)
	refundService := refundUsecase.NewService(unit, refundRequests, orders, events, paymentService, dispatcher)
	// And back the other way, which is why it is a second statement rather
	// than a constructor argument: settlement opens the box office's own refund
	// when it finds the tickets gone, and the refund service is built from the
	// payment service, so the two are joined here after both exist.
	paymentService.WithRefunds(refundService)
	// And the expiring hold takes its payment code with it, which is the same
	// shape again: checkout declares the port, payment implements it, the
	// container is the only place that knows both.
	checkoutService.WithChargeVoider(paymentService)
	reportService := reportUsecase.NewService(reportRepository.NewReportRepository(db), events)

	// The door. The repository above serves reads and scans; issuing goes
	// through the unit of work so the tickets commit with the payment that
	// bought them.
	admissionService := admissionUsecase.NewService(admissions, orders, events).
		WithCodeRenderer(qrcode.NewRenderer())

	// Reserved seating. The layout side is an editor and reads and writes
	// outside any transaction; the seat side is inventory and its writes go
	// through the unit of work, which is why the two are separate repositories
	// bound to the same connection here.
	seatingService := seatingUsecase.NewService(
		seatingRepository.NewLayoutRepository(db),
		seatingRepository.NewSeatRepository(db),
		events,
		prefixedID,
	)

	checkoutHandler := checkoutHTTP.NewHandler(
		checkoutService, paymentService, eventService, refundService,
		keys, cfg.Queue.IdempotencyLease,
	)
	refundHandler := refundHTTP.NewHandler(refundService)
	reportHandler := reportHTTP.NewHandler(reportService)
	admissionHandler := admissionHTTP.NewHandler(admissionService)
	seatingHandler := seatingHTTP.NewHandler(seatingService)

	// Said once at boot, because it is the number every receipt, payout and
	// refund in this process will be computed from. An operator reconciling a
	// disputed charge should be able to read the rate that produced it out of
	// the log for that deploy rather than having to match a build to a commit.
	if fee := cfg.ServiceFee; fee.Free() {
		log.Printf("pricing: service fee is ZERO - buyers pay the tier price and no commission is taken. This is a build-level setting; check domain/pricing.PlatformBasisPoints")
	} else {
		log.Printf("pricing: service fee %d basis points (%d.%02d%%) added on top of every paid ticket; the organiser is owed the face value",
			fee.BasisPoints, fee.BasisPoints/100, fee.BasisPoints%100)
	}

	webhookHandler := buildWebhookHandler(cfg.Payments, jobs, dispatcher)
	switch {
	case !cfg.Payments.Enabled() && cfg.Payments.Provider == paymentdomain.ProviderMercadoPago:
		log.Printf("payments: Mercado Pago is not configured; checkout will answer 503 until MERCADOPAGO_ACCESS_TOKEN is set")
	case !cfg.Payments.Enabled():
		log.Printf("payments: Asaas is not configured; checkout will answer 503 until ASAAS_API_KEY is set")
	case cfg.Payments.Provider == paymentdomain.ProviderMercadoPago:
		log.Printf("payments: Mercado Pago enabled, holds last %s", cfg.Payments.HoldFor)
	default:
		// The environment is stated plainly, because the sandbox is a different
		// HOST rather than a flag and "am I billing real money" is the first
		// question anybody has when a charge behaves unexpectedly.
		mode := "PRODUCTION (charges are real)"
		if asaas.NewClient(cfg.Payments.AsaasAPIKey, cfg.Payments.AsaasBaseURL).Sandbox() {
			mode = "SANDBOX"
		}
		log.Printf("payments: Asaas enabled in %s mode, holds last %s", mode, cfg.Payments.HoldFor)
	}

	var checkoutLimit, authLimit *authMiddleware.RateLimit
	if limiter != nil {
		checkoutLimit = authMiddleware.NewRateLimit(limiter, "checkout", cfg.Cache.CheckoutRateLimit, cfg.Cache.CheckoutRateWindow, clients)
		authLimit = authMiddleware.NewRateLimit(limiter, "auth", cfg.Cache.AuthRateLimit, cfg.Cache.AuthRateWindow, clients)
	}
	checkoutAdmission := authMiddleware.NewConcurrencyLimit(cfg.CheckoutMaxInFlight)

	// Metrics. Built here and handed both to the router, which measures
	// requests, and to the sweeps below, which publish queue depth and the
	// order statuses worth alerting on.
	metrics := prometheusMetrics.New(prometheusMetrics.ReplicaID())
	container.recorder = metrics
	container.metrics = newMetricsServer(cfg.MetricsListenAddr, metrics.Handler())

	router := delivery.NewRouter(delivery.Dependencies{
		Auth:              authHandler,
		Tickets:           ticketHandler,
		Events:            eventHandler,
		Checkout:          checkoutHandler,
		Refunds:           refundHandler,
		Payouts:           payoutHTTP.NewHandler(payoutService),
		Reports:           reportHandler,
		Admissions:        admissionHandler,
		Seating:           seatingHandler,
		Webhooks:          webhookHandler,
		AuthMiddleware:    authGuard,
		CheckoutLimit:     checkoutLimit,
		CheckoutAdmission: checkoutAdmission,
		AuthLimit:         authLimit,
		Metrics:           authMiddleware.NewMetrics(metrics),
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

	container.startWorkers(cfg, jobs, orders, keys, consumer, dispatcher, checkoutService, paymentService, messenger)
	return container, nil
}

// buildNotifications assembles the messaging stack, or reports that there is
// none.
//
// Everything is decided here and nowhere else: which provider carries email,
// which channels therefore exist, and what the notifier is allowed to queue.
// With no RESEND_API_KEY it returns nils all the way down, a supported state
// in development, and one config.loadNotifications refuses in production,
// because a box office that takes money and tells nobody is worse than one
// that will not start.
func buildNotifications(
	cfg config.NotificationsConfig,
	jobs queuedomain.Queue,
	dispatcher *queueUsecase.Dispatcher,
	// tickets puts the QR and the printed code into a confirmation. Nil is
	// supported and means the receipt goes out without them, which is what a
	// deployment with no encryption keyring gets: a receipt, never a failed
	// send.
	tickets notificationUsecase.Tickets,
) (*notificationUsecase.Service, *notificationUsecase.Purchases, *notificationUsecase.Notifier, error) {
	// One stack, assembled from whatever is configured. Each channel contributes
	// a Sender and the Renderer that produces the body it carries, and the two are
	// added together so a channel can never be queueable without being renderable.
	renderers := map[notificationdomain.Channel]notificationdomain.Renderer{}
	var senders []notificationdomain.Sender

	email := notifications.NewEmailSender(cfg)
	if email == nil {
		log.Printf("notifications: RESEND_API_KEY is not set; buyers receive no order emails")
	}

	if email != nil {
		// Parsed once, at boot. A template with a syntax error or a missing
		// component fails the deploy here rather than one buyer's receipt later.
		renderer, err := notifications.NewRenderer(cfg.Brand)
		if err != nil {
			return nil, nil, nil, err
		}
		renderers[email.Channel()] = renderer
		senders = append(senders, email)
		log.Printf("notifications: Resend enabled, %d template(s), from %q, up to %d/s per replica",
			renderer.Templates(), cfg.FromEmail, cfg.MaxRPS)
	}

	// Codes to a phone, carried by Vozko. Independent of email on purpose: this
	// was one early return on RESEND_API_KEY, which would have meant a box office
	// with no mail provider could not confirm a phone either, for no reason beyond
	// the order the two were wired in.
	if codes := notifications.NewVozkoCodeSender(cfg.Phone); codes != nil {
		// The wording of a code message is not ours: an approved WhatsApp
		// authentication template is fixed text at Meta, so what is rendered for
		// this channel is the code itself. See notifications.PhoneCodeRenderer.
		renderers[codes.Channel()] = notifications.PhoneCodeRenderer{}
		senders = append(senders, codes)
		log.Printf("notifications: verification codes over %s via Vozko at %s (workspace %s), up to %d/s per replica",
			codes.Channel(), cfg.Phone.BaseURL, cfg.Phone.WorkspaceID, cfg.Phone.MaxRPS)
	}

	if len(senders) == 0 {
		return nil, nil, nil, nil
	}

	messenger := notificationUsecase.NewService(notifications.NewChannels(renderers), senders...).
		WithTickets(tickets)
	notifier := notificationUsecase.NewNotifier(jobs, dispatcher, messenger.Channels()...)
	purchases := notificationUsecase.NewPurchases(notifier, cfg.Brand.SiteURL)

	return messenger, purchases, notifier, nil
}

// buildGateway returns the payment adapter, or nil when the provider is not
// configured. A nil gateway is a supported state: the box office still manages
// tickets, and checkout answers "payment provider is not configured".
// buildGateway resolves PAYMENT_PROVIDER into a concrete adapter.
//
// This function and the webhook selection below it are the ONLY places in the
// codebase that branch on which provider is active. Everything downstream:
// checkout, settlement, refunds, the reconciliation sweep, depends on the
// payment.Gateway port and cannot tell the difference.
func buildGateway(cfg config.PaymentsConfig) paymentdomain.Gateway {
	if !cfg.Enabled() {
		return nil
	}

	switch cfg.Provider {
	case paymentdomain.ProviderMercadoPago:
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

	default:
		return asaas.NewGateway(asaas.NewClient(cfg.AsaasAPIKey, cfg.AsaasBaseURL))
	}
}

// buildWebhookHandler mounts the callback route for the active provider.
func buildWebhookHandler(
	cfg config.PaymentsConfig,
	jobs queuedomain.Queue,
	dispatcher *queueUsecase.Dispatcher,
) delivery.WebhookRegistrar {
	if !cfg.Enabled() {
		return nil
	}
	switch cfg.Provider {
	case paymentdomain.ProviderMercadoPago:
		return webhooksHTTP.NewMercadoPagoHandler(jobs, dispatcher, cfg.WebhookSecret, cfg.SignatureTolerance)
	default:
		return webhooksHTTP.NewAsaasHandler(jobs, dispatcher, cfg.AsaasWebhookToken)
	}
}

// startWorkers runs the queue in this process.
//
// In-process by default because it is one binary to deploy and the work is
// small; the same worker runs standalone by starting the binary with the HTTP
// server disabled, and nothing in the design assumes a single node, claiming
// is exclusive at the database.
func (c *Container) startWorkers(
	cfg config.Config,
	jobs queuedomain.Queue,
	orders orderdomain.Repository,
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
		options = append(options, queueUsecase.WithMetrics(c.recorder))
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
			// timer of their own, and keeping them afterwards would be
			// retaining a record of who signed in and when, which is exactly
			// what that table's shape avoids holding.
			//
			// This runs every minute while a challenge is kept for an hour, so
			// the sweep is mostly a no-op and deliberately so: the row outlives
			// its own code by design, because the per-destination ceiling is
			// counted from those rows. See SweepOldChallenges.
			if c.challenges != nil {
				if _, sweepErr := c.challenges.SweepOldChallenges(ctx, 500); sweepErr != nil {
					log.Printf("auth: sweeping old challenges: %v", sweepErr)
				}
			}
			// The gauges ride this sweep too, for the same reason the codes do:
			// a minute is the resolution an operator needs and a timer of its
			// own would be a second thing to start, stop and get wrong.
			//
			// Published from ONE worker's sweep and not from every replica: the
			// numbers are table-wide counts, so each replica would otherwise
			// publish the same figure under its own replica_id and a naive
			// sum() across replicas would multiply the backlog by the fleet
			// size. The dedupe key on the sweep job is what makes it one.
			c.publishGauges(ctx, jobs, orders)
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

		worker.Handle(queuedomain.TypeCancelCharge, func(ctx context.Context, job queuedomain.Job) error {
			payload, err := queueUsecase.Decode[queuedomain.SyncPaymentPayload](job)
			if err != nil {
				return err
			}
			return payments.CancelCharge(ctx, payload.OrderID)
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

// publishGauges puts the queue depth and the order statuses worth alerting on
// in front of Prometheus.
//
// Counts rather than events, because these are questions about NOW: how much
// work is waiting, and how many buyers are owed their money back. A counter
// would answer "how many ever", which is not what wakes somebody up.
//
// Every failure here is logged and swallowed. A monitoring read that took down
// the sweep it rides on would cost expired holds their release, which is stock
// nobody can buy; being unable to publish a number is never worth that.
func (c *Container) publishGauges(ctx context.Context, jobs queuedomain.Queue, orders orderdomain.Repository) {
	if c.recorder == nil {
		return
	}
	for _, status := range []queuedomain.Status{
		queuedomain.StatusPending, queuedomain.StatusProcessing, queuedomain.StatusDead,
	} {
		count, err := jobs.CountByStatus(ctx, status)
		if err != nil {
			log.Printf("metrics: counting %s jobs: %v", status, err)
			continue
		}
		c.recorder.SetQueueJobs(string(status), count)
	}

	// refund_required only. It is the one status that means money was taken
	// for tickets that no longer exist, so anything above zero is a person
	// owed a refund; the rest of the statuses are already on the report pages
	// and counting them all here would be a query per status per minute for
	// numbers nobody alerts on.
	count, err := orders.Count(ctx, orderdomain.Filter{Status: orderdomain.StatusRefundRequired})
	if err != nil {
		log.Printf("metrics: counting refund_required orders: %v", err)
		return
	}
	c.recorder.SetOrdersByStatus(string(orderdomain.StatusRefundRequired), count)
}

func (c *Container) Start() error {
	// The scrape listener first, and in the background: it is a separate
	// server, so a port already in use there must not stop the API from
	// serving buyers. Nil-safe when METRICS_LISTEN_ADDR is empty.
	c.metrics.Start()
	return c.server.ListenAndServe()
}

func (c *Container) Shutdown(ctx context.Context) error {
	serverErr := c.server.Shutdown(ctx)
	c.metrics.Shutdown(ctx)
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

// prefixedID mints an identifier in the shape the rest of this codebase uses:
// a short type prefix, an underscore, sixteen hex characters.
//
// Passed into the seating use case rather than reached for inside it, because a
// use case that generated its own ids could not be tested for the labels it
// produces without matching random strings. It is the same argument the payment
// and notification services already make for their own newID.
func prefixedID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		// A clock-based fallback rather than a panic. Losing entropy makes an
		// id guessable, which matters for a code somebody scans at a door and
		// not at all for the primary key of a row in a layout.
		return prefix + "_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}
