package config

import (
	"fmt"
	"log"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppName           string
	Port              string
	CORSAllowedOrigin string
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// ShutdownTimeout bounds the orderly stop: draining HTTP connections, then
	// letting workers finish the jobs already in their hands.
	//
	// It must exceed QUEUE_JOB_TIMEOUT for a worker mid-charge to finish, and
	// the orchestrator's own grace period (Kubernetes defaults to 30s) must
	// exceed THIS, or the process is killed halfway through its own drain.
	ShutdownTimeout     time.Duration
	JWTSecret           string
	AccessTokenTTL      time.Duration
	RefreshTokenTTL     time.Duration
	CookieDomain        string
	CookieSecure        bool
	CheckoutMaxInFlight int
	// CheckoutMaxOpenOrders and CheckoutMaxHeldPerTier cap what ONE account may
	// keep reserved and unpaid. Zero disables that dimension.
	CheckoutMaxOpenOrders  int
	CheckoutMaxHeldPerTier int
	// TrustedProxyCIDRs are the networks whose X-Forwarded-For header may be
	// believed. Everything else is rate limited by the address it connected
	// from, because the header is otherwise client-controlled text.
	TrustedProxyCIDRs []string
	// MediaProcessors bounds concurrent image resizing. A pure-Go resample of a
	// large photo is hundreds of milliseconds and tens of megabytes, so this is
	// what turns a burst of uploads into a queue rather than an out-of-memory
	// kill.
	MediaProcessors int
	Database        DatabaseConfig
	Media           MediaConfig
	Payments        PaymentsConfig
	Queue           QueueConfig
	Broker          BrokerConfig
	Cache           CacheConfig
	Notifications   NotificationsConfig
}

// BrokerConfig points the job transport at RabbitMQ.
//
// Optional: with no URL the durable queue still runs on its poller, which is
// slower but correct. That is what lets a developer clone this and run it
// without a broker.
type BrokerConfig struct {
	URL       string
	Prefetch  int
	QueueType string
}

func (b BrokerConfig) Enabled() bool { return strings.TrimSpace(b.URL) != "" }

// CacheConfig points the read cache and the rate limiter at Redis.
//
// Also optional, and with the same reasoning: without it every read goes to
// PostgreSQL and the rate limiter is off. Both are load shedding, not
// correctness.
type CacheConfig struct {
	URL       string
	KeyPrefix string
	// CheckoutRateLimit is how many checkouts one caller may start per window.
	CheckoutRateLimit  int
	CheckoutRateWindow time.Duration
	// AuthRateLimit caps the unauthenticated credential routes per client
	// address. Each login is a bcrypt compare, so an unthrottled flood is both
	// a credential-stuffing surface and a CPU denial of service.
	AuthRateLimit  int
	AuthRateWindow time.Duration
	// LoginEmailRateLimit caps attempts against ONE account, whatever address
	// they come from. The address limit alone lets a botnet spread a guessing
	// run over thousands of hosts and never trip it.
	LoginEmailRateLimit int
	// SessionCacheTTL is how long "this access token's session is live" is
	// believed without asking PostgreSQL.
	//
	// It is the single busiest query in the system: every authenticated request
	// makes it, and a checkout screen polls for its PIX code for the length of a
	// thirty-minute hold. Thirty seconds collapses fifteen polls into one read
	// while keeping a logout's effect bounded by half a minute in the worst
	// case — and a logout invalidates the entry outright, so the bound only
	// applies if the cache dropped the key some other way. Zero disables it.
	SessionCacheTTL time.Duration
}

func (c CacheConfig) Enabled() bool { return strings.TrimSpace(c.URL) != "" }

// NotificationsConfig is the transactional messaging integration: Resend for
// email today, and whatever channel is added next without this struct changing
// shape beyond one more block.
//
// Unlike the cache and the broker, this one is NOT optional in production. A
// missing RESEND_API_KEY does not degrade the product — it silently stops
// telling buyers that their money arrived, and the first person to notice is a
// customer who believes the box office took their money and vanished. Refusing
// to start is the honest failure; see loadNotifications.
type NotificationsConfig struct {
	ResendAPIKey string
	FromEmail    string
	FromName     string
	// ReplyTo points a buyer who hits reply at a human instead of at a
	// no-reply black hole.
	ReplyTo string
	// MaxRPS caps the client-side send rate. Resend documents five requests a
	// second per team; staying under it turns a burst of confirmations into a
	// short queue rather than a wall of 429s.
	MaxRPS int
	Brand  BrandConfig
}

// Enabled reports whether messages can be delivered at all. Off, the box office
// still sells: orders settle, stock moves, and nothing is queued that could
// never be sent.
func (n NotificationsConfig) Enabled() bool { return strings.TrimSpace(n.ResendAPIKey) != "" }

// BrandConfig is the identity every message carries, read from the same BRAND_*
// variables Vozko's backend uses so one set of values configures both.
//
// Configuration and not constants because no template may hardcode a name: the
// same binary runs as another box office by changing the environment, exactly
// as the media and payment adapters already do here.
type BrandConfig struct {
	Name         string
	LegalName    string
	CNPJ         string
	SiteURL      string
	SupportEmail string
	LogoURL      string
}

// PaymentsConfig is the Mercado Pago integration, using the same variable names
// as Vozko's backend so one account's credentials serve both.
type PaymentsConfig struct {
	AccessToken   string
	WebhookSecret string
	BaseURL       string
	// NotificationURL is set per payment rather than account-wide, which is
	// what guarantees the data.id query parameter the webhook signature is
	// computed over.
	NotificationURL string
	// SignatureTolerance bounds how old a webhook signature may be. Zero, the
	// default, disables the window: Mercado Pago retries a failed delivery for
	// hours without re-signing it.
	SignatureTolerance time.Duration
	// SandboxPayerEmail redirects every charge to a provider test user. It is
	// honoured only in development, because in production it would put someone
	// else's address on a real charge.
	SandboxPayerEmail string
	// HoldFor is how long a reservation survives without payment, once the
	// buyer has confirmed their details.
	HoldFor time.Duration
	// CartHoldFor is the shorter window a basket gets between reaching the
	// details form and submitting it.
	CartHoldFor time.Duration
}

// Enabled reports whether charges can be issued at all. Without a token the
// box office still runs: tickets are managed, and checkout answers 503.
func (p PaymentsConfig) Enabled() bool { return strings.TrimSpace(p.AccessToken) != "" }

// QueueConfig tunes the durable worker.
type QueueConfig struct {
	Workers            int
	PollInterval       time.Duration
	BatchSize          int
	ReconcileBatchSize int
	// AuditBatchSize is how many settled orders one hourly audit re-reads at the
	// provider. It catches a refund or chargeback whose notification was lost,
	// which the pending-payment sweep can never see.
	AuditBatchSize   int
	CleanupBatchSize int
	// IdempotencyLease is how long a claimed but unfinished request keeps its
	// key. A process that dies mid-checkout otherwise answers every retry with
	// "still in progress" for the full 24-hour key lifetime.
	IdempotencyLease   time.Duration
	CompletedRetention time.Duration
	// JobTimeout bounds one attempt at a job. It must stay under the worker's
	// stale window, so a slow job is never still running when the sweep
	// decides its worker died and hands the row to somebody else.
	JobTimeout time.Duration
	// SweepInterval is how often the hold-expiry and reconciliation sweeps are
	// scheduled.
	SweepInterval time.Duration
}

// MediaConfig points the object store at Cloudflare R2, using the same variable
// names Vozko's backend reads so one set of credentials serves both.
//
// When the R2 credentials are absent the application falls back to a directory
// on disk, which is what makes a fresh clone runnable without a Cloudflare
// account. Production refuses that fallback: see loadMedia.
type MediaConfig struct {
	AccountID     string
	AccessKeyID   string
	SecretKey     string
	Bucket        string
	PublicBaseURL string
	// KeyPrefix namespaces every object this application writes. It is what
	// makes sharing one bucket with another product safe: without it, two
	// applications writing "tickets/..." would be writing over each other.
	KeyPrefix  string
	LocalDir   string
	LocalRoute string
}

// UsesR2 reports whether every credential R2 needs is present. A partial set is
// treated as absent, because half-configured credentials fail at upload time,
// which is the worst moment to discover them.
func (m MediaConfig) UsesR2() bool {
	return m.AccountID != "" && m.AccessKeyID != "" && m.SecretKey != "" && m.Bucket != ""
}

type DatabaseConfig struct {
	Host            string
	Port            string
	User            string
	Password        string
	Name            string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

func Load() (Config, error) {
	readTimeout, err := duration("READ_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := duration("WRITE_TIMEOUT", 20*time.Second)
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := duration("IDLE_TIMEOUT", 60*time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := duration("SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	accessTTL, err := duration("ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	refreshTTL, err := duration("REFRESH_TOKEN_TTL", 30*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		if os.Getenv("APP_ENV") == "production" {
			return Config{}, fmt.Errorf("JWT_SECRET is required in production")
		}
		jwtSecret = "development-only-secret-change-me-before-production"
	}
	cookieSecure, err := boolean("COOKIE_SECURE", os.Getenv("APP_ENV") == "production")
	if err != nil {
		return Config{}, err
	}
	mediaProcessors, err := integer("MEDIA_PROCESSORS", 2)
	if err != nil {
		return Config{}, err
	}
	checkoutMaxInFlight, err := integer("CHECKOUT_MAX_IN_FLIGHT", 20)
	if err != nil {
		return Config{}, err
	}
	// Three open orders and ten tickets of one tier: enough for a buyer holding
	// Pista and Camarote while they decide, far short of an account that can
	// keep an event off the shelf for free.
	maxOpenOrders, err := integer("CHECKOUT_MAX_OPEN_ORDERS", 3)
	if err != nil {
		return Config{}, err
	}
	maxHeldPerTier, err := integer("CHECKOUT_MAX_HELD_PER_TIER", 10)
	if err != nil {
		return Config{}, err
	}
	database, err := loadDatabase()
	if err != nil {
		return Config{}, err
	}
	port := value("PORT", "8080")
	mediaConfig, err := loadMedia(port)
	if err != nil {
		return Config{}, err
	}
	payments, err := loadPayments()
	if err != nil {
		return Config{}, err
	}
	queueConfig, err := loadQueue()
	if err != nil {
		return Config{}, err
	}
	broker, err := loadBroker()
	if err != nil {
		return Config{}, err
	}
	cacheConfig, err := loadCache()
	if err != nil {
		return Config{}, err
	}
	notifications, err := loadNotifications(value("CORS_ALLOW_ORIGIN", "http://localhost:3000"))
	if err != nil {
		return Config{}, err
	}

	return Config{
		AppName:                value("APP_NAME", "Vozkot API"),
		Port:                   port,
		CORSAllowedOrigin:      value("CORS_ALLOW_ORIGIN", "http://localhost:3000"),
		ReadTimeout:            readTimeout,
		WriteTimeout:           writeTimeout,
		IdleTimeout:            idleTimeout,
		ShutdownTimeout:        shutdownTimeout,
		JWTSecret:              jwtSecret,
		AccessTokenTTL:         accessTTL,
		RefreshTokenTTL:        refreshTTL,
		CookieDomain:           os.Getenv("COOKIE_DOMAIN"),
		CookieSecure:           cookieSecure,
		CheckoutMaxInFlight:    checkoutMaxInFlight,
		CheckoutMaxOpenOrders:  maxOpenOrders,
		CheckoutMaxHeldPerTier: maxHeldPerTier,
		TrustedProxyCIDRs:      trustedProxies(),
		MediaProcessors:        mediaProcessors,
		Database:               database,
		Media:                  mediaConfig,
		Payments:               payments,
		Queue:                  queueConfig,
		Broker:                 broker,
		Cache:                  cacheConfig,
		Notifications:          notifications,
	}, nil
}

// DefaultTrustedProxyCIDRs are the private ranges a load balancer or ingress
// sits in.
//
// The default is not "trust nothing": behind a load balancer that would make
// every request share one client address and turn the per-address limit into a
// single global counter. It is not "trust everything" either, which would let
// any caller pick their own rate-limit bucket with a header. Trusting private
// ranges means a spoofed X-Forwarded-For counts only if the connection itself
// came from inside the network, which an attacker on the internet cannot
// arrange.
var DefaultTrustedProxyCIDRs = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"::1/128", "fc00::/7",
}

// trustedProxies reads the override, where an explicitly empty value means
// "trust no proxy" and is honoured as such.
func trustedProxies() []string {
	raw, present := os.LookupEnv("TRUSTED_PROXY_CIDRS")
	if !present {
		return DefaultTrustedProxyCIDRs
	}
	parts := strings.Split(raw, ",")
	cidrs := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			cidrs = append(cidrs, trimmed)
		}
	}
	return cidrs
}

func loadBroker() (BrokerConfig, error) {
	prefetch, err := integer("RABBITMQ_PREFETCH", 16)
	if err != nil {
		return BrokerConfig{}, err
	}
	if prefetch == 0 {
		prefetch = 16
	}
	queueType := strings.ToLower(value("RABBITMQ_QUEUE_TYPE", "classic"))
	if queueType != "classic" && queueType != "quorum" {
		return BrokerConfig{}, fmt.Errorf("RABBITMQ_QUEUE_TYPE must be classic or quorum")
	}
	return BrokerConfig{
		URL:       strings.TrimSpace(os.Getenv("RABBITMQ_URL")),
		Prefetch:  prefetch,
		QueueType: queueType,
	}, nil
}

func loadCache() (CacheConfig, error) {
	limit, err := integer("CHECKOUT_RATE_LIMIT", 30)
	if err != nil {
		return CacheConfig{}, err
	}
	window, err := duration("CHECKOUT_RATE_WINDOW", time.Minute)
	if err != nil {
		return CacheConfig{}, err
	}
	authLimit, err := integer("AUTH_RATE_LIMIT", 20)
	if err != nil {
		return CacheConfig{}, err
	}
	authWindow, err := duration("AUTH_RATE_WINDOW", time.Minute)
	if err != nil {
		return CacheConfig{}, err
	}
	loginEmailLimit, err := integer("LOGIN_EMAIL_RATE_LIMIT", 10)
	if err != nil {
		return CacheConfig{}, err
	}
	sessionTTL, err := duration("SESSION_CACHE_TTL", 30*time.Second)
	if err != nil {
		return CacheConfig{}, err
	}
	return CacheConfig{
		URL:                 strings.TrimSpace(os.Getenv("REDIS_URL")),
		KeyPrefix:           value("REDIS_KEY_PREFIX", "vozkot"),
		CheckoutRateLimit:   limit,
		CheckoutRateWindow:  window,
		AuthRateLimit:       authLimit,
		AuthRateWindow:      authWindow,
		LoginEmailRateLimit: loginEmailLimit,
		SessionCacheTTL:     sessionTTL,
	}, nil
}

// loadNotifications resolves the messaging integration and the brand every
// message wears.
//
// siteURL defaults to the frontend origin the API already knows about, which
// is what makes a fresh clone produce working links and a working logo before
// anybody has set a BRAND_ variable.
func loadNotifications(frontendOrigin string) (NotificationsConfig, error) {
	maxRPS, err := integer("RESEND_MAX_REQUESTS_PER_SECOND", 4)
	if err != nil {
		return NotificationsConfig{}, err
	}

	brand := BrandConfig{
		Name:         value("BRAND_NAME", "Vozko Tickets"),
		LegalName:    strings.TrimSpace(os.Getenv("BRAND_LEGAL_NAME")),
		CNPJ:         strings.TrimSpace(os.Getenv("BRAND_CNPJ")),
		SiteURL:      strings.TrimRight(value("BRAND_SITE_URL", frontendOrigin), "/"),
		SupportEmail: strings.TrimSpace(os.Getenv("BRAND_SUPPORT_EMAIL")),
	}
	// The frontend serves the mark at a fixed path, so the default is right
	// without configuration and still overridable when the logo moves to a CDN.
	brand.LogoURL = value("BRAND_LOGO_URL", brand.SiteURL+"/brand/vozko-tickets-logo.png")

	notifications := NotificationsConfig{
		ResendAPIKey: strings.TrimSpace(os.Getenv("RESEND_API_KEY")),
		FromEmail:    strings.TrimSpace(value("RESEND_FROM_EMAIL", os.Getenv("BRAND_FROM_EMAIL"))),
		FromName:     strings.TrimSpace(value("RESEND_FROM_NAME", brand.Name)),
		ReplyTo:      strings.TrimSpace(value("RESEND_REPLY_TO", brand.SupportEmail)),
		MaxRPS:       maxRPS,
		Brand:        brand,
	}

	// A From address that the provider rejects fails EVERY send, and it fails
	// them in a worker rather than at boot, where nobody is looking. One parse
	// at startup turns a silent outage into a refused deploy.
	for label, address := range map[string]string{
		"RESEND_FROM_EMAIL": notifications.FromEmail,
		"RESEND_REPLY_TO":   notifications.ReplyTo,
	} {
		if address == "" {
			continue
		}
		if _, err := mail.ParseAddress(address); err != nil {
			return NotificationsConfig{}, fmt.Errorf("%s is not a valid email address: %w", label, err)
		}
	}

	if notifications.Enabled() && notifications.FromEmail == "" {
		return NotificationsConfig{}, fmt.Errorf("RESEND_FROM_EMAIL (or BRAND_FROM_EMAIL) is required when RESEND_API_KEY is set")
	}
	if os.Getenv("APP_ENV") == "production" {
		// Not a preference. An unconfigured sender in production means every
		// buyer pays and hears nothing back, which the system cannot detect on
		// its own because no job fails: none are ever written.
		if !notifications.Enabled() {
			return NotificationsConfig{}, fmt.Errorf("RESEND_API_KEY is required in production; buyers must receive their order confirmations")
		}
		if brand.SiteURL == "" {
			return NotificationsConfig{}, fmt.Errorf("BRAND_SITE_URL is required in production")
		}
	}
	return notifications, nil
}

func loadPayments() (PaymentsConfig, error) {
	tolerance, err := duration("MERCADOPAGO_SIGNATURE_TOLERANCE", 0)
	if err != nil {
		return PaymentsConfig{}, err
	}
	holdFor, err := duration("CHECKOUT_HOLD_TTL", 30*time.Minute)
	if err != nil {
		return PaymentsConfig{}, err
	}
	// The window a basket gets before the buyer has told us anything. It is
	// deliberately far shorter than the payment window: nothing has been asked
	// of the buyer yet, so stock held on the strength of a page view should
	// cost the event as little as possible.
	cartHoldFor, err := duration("CHECKOUT_CART_TTL", 10*time.Minute)
	if err != nil {
		return PaymentsConfig{}, err
	}

	payments := PaymentsConfig{
		AccessToken:        strings.TrimSpace(os.Getenv("MERCADOPAGO_ACCESS_TOKEN")),
		WebhookSecret:      strings.TrimSpace(os.Getenv("MERCADOPAGO_WEBHOOK_SECRET")),
		BaseURL:            strings.TrimSpace(os.Getenv("MERCADOPAGO_BASE_URL")),
		NotificationURL:    strings.TrimRight(strings.TrimSpace(os.Getenv("MERCADOPAGO_NOTIFICATION_URL")), "/"),
		SignatureTolerance: tolerance,
		HoldFor:            holdFor,
		CartHoldFor:        cartHoldFor,
	}

	// A configured provider without a webhook secret is the dangerous shape:
	// the endpoint would have to either reject every notification or trust
	// unsigned ones. Refusing to start is the only safe answer.
	if payments.Enabled() && payments.WebhookSecret == "" {
		return PaymentsConfig{}, fmt.Errorf("MERCADOPAGO_WEBHOOK_SECRET is required when MERCADOPAGO_ACCESS_TOKEN is set")
	}
	if payments.Enabled() && payments.NotificationURL == "" && os.Getenv("APP_ENV") == "production" {
		return PaymentsConfig{}, fmt.Errorf("MERCADOPAGO_NOTIFICATION_URL is required in production")
	}

	payments.SandboxPayerEmail = sandboxPayerEmail()
	return payments, nil
}

// sandboxPayerEmail resolves the development-only payer override, refusing it
// anywhere else.
func sandboxPayerEmail() string {
	email := strings.TrimSpace(os.Getenv("MERCADOPAGO_SANDBOX_PAYER_EMAIL"))
	if email == "" {
		return ""
	}
	if os.Getenv("APP_ENV") != "development" {
		log.Printf("config: ignoring MERCADOPAGO_SANDBOX_PAYER_EMAIL; it is honoured only when APP_ENV=development")
		return ""
	}
	log.Printf("config: charges will be addressed to the sandbox payer %s instead of the real buyer", email)
	return email
}

func loadQueue() (QueueConfig, error) {
	workers, err := integer("QUEUE_WORKERS", 2)
	if err != nil {
		return QueueConfig{}, err
	}
	if workers == 0 {
		workers = 1
	}
	batch, err := integer("QUEUE_BATCH_SIZE", 10)
	if err != nil {
		return QueueConfig{}, err
	}
	if batch == 0 {
		batch = 10
	}
	poll, err := duration("QUEUE_POLL_INTERVAL", time.Second)
	if err != nil {
		return QueueConfig{}, err
	}
	sweep, err := duration("QUEUE_SWEEP_INTERVAL", time.Minute)
	if err != nil {
		return QueueConfig{}, err
	}
	jobTimeout, err := duration("QUEUE_JOB_TIMEOUT", 2*time.Minute)
	if err != nil {
		return QueueConfig{}, err
	}
	reconcileBatch, err := integer("QUEUE_RECONCILE_BATCH_SIZE", 1000)
	if err != nil {
		return QueueConfig{}, err
	}
	if reconcileBatch == 0 {
		reconcileBatch = 1000
	}
	auditBatch, err := integer("QUEUE_AUDIT_BATCH_SIZE", 500)
	if err != nil {
		return QueueConfig{}, err
	}
	if auditBatch == 0 {
		auditBatch = 500
	}
	cleanupBatch, err := integer("QUEUE_CLEANUP_BATCH_SIZE", 10000)
	if err != nil {
		return QueueConfig{}, err
	}
	if cleanupBatch == 0 {
		cleanupBatch = 10000
	}
	idempotencyLease, err := duration("IDEMPOTENCY_LEASE", time.Minute)
	if err != nil {
		return QueueConfig{}, err
	}
	completedRetention, err := duration("QUEUE_COMPLETED_RETENTION", 7*24*time.Hour)
	if err != nil {
		return QueueConfig{}, err
	}
	return QueueConfig{
		Workers:            workers,
		PollInterval:       poll,
		BatchSize:          batch,
		ReconcileBatchSize: reconcileBatch,
		AuditBatchSize:     auditBatch,
		CleanupBatchSize:   cleanupBatch,
		IdempotencyLease:   idempotencyLease,
		CompletedRetention: completedRetention,
		SweepInterval:      sweep,
		JobTimeout:         jobTimeout,
	}, nil
}

func loadMedia(port string) (MediaConfig, error) {
	media := MediaConfig{
		AccountID:     os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
		AccessKeyID:   os.Getenv("CLOUDFLARE_R2_KEY_ID"),
		SecretKey:     os.Getenv("CLOUDFLARE_R2_SECRET_KEY"),
		Bucket:        os.Getenv("CLOUDFLARE_R2_BUCKET_NAME"),
		PublicBaseURL: strings.TrimRight(os.Getenv("CLOUDFLARE_R2_ENDPOINT"), "/"),
		KeyPrefix:     strings.Trim(strings.TrimSpace(os.Getenv("CLOUDFLARE_R2_KEY_PREFIX")), "/"),
		LocalDir:      value("MEDIA_LOCAL_DIR", filepath.Join("storage", "media")),
		LocalRoute:    "/media/",
	}
	if os.Getenv("APP_ENV") == "production" {
		for key, present := range map[string]string{
			"CLOUDFLARE_ACCOUNT_ID":     media.AccountID,
			"CLOUDFLARE_R2_KEY_ID":      media.AccessKeyID,
			"CLOUDFLARE_R2_SECRET_KEY":  media.SecretKey,
			"CLOUDFLARE_R2_BUCKET_NAME": media.Bucket,
			"CLOUDFLARE_R2_ENDPOINT":    media.PublicBaseURL,
		} {
			if present == "" {
				return MediaConfig{}, fmt.Errorf("%s is required in production", key)
			}
		}
	}
	if media.PublicBaseURL == "" {
		// Local development serves the same bytes from the API itself, so the
		// URLs stored on media rows stay absolute and the frontend needs no
		// special case for "this one is a dev file".
		media.PublicBaseURL = strings.TrimRight(value("MEDIA_PUBLIC_BASE_URL", "http://localhost:"+port+"/media"), "/")
	}
	return media, nil
}

func loadDatabase() (DatabaseConfig, error) {
	if os.Getenv("APP_ENV") == "production" {
		for _, key := range []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME"} {
			if os.Getenv(key) == "" {
				return DatabaseConfig{}, fmt.Errorf("%s is required in production", key)
			}
		}
	}
	// Enough for the default workers' in-flight jobs plus a modest HTTP load;
	// the sizing rule is in docs/SCALE.md.
	maxOpen, err := integer("DB_MAX_OPEN_CONNS", 25)
	if err != nil {
		return DatabaseConfig{}, err
	}
	maxIdle, err := integer("DB_MAX_IDLE_CONNS", 10)
	if err != nil {
		return DatabaseConfig{}, err
	}
	maxLifetime, err := duration("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		return DatabaseConfig{}, err
	}
	maxIdleTime, err := duration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute)
	if err != nil {
		return DatabaseConfig{}, err
	}
	return DatabaseConfig{
		Host:            value("DB_HOST", "localhost"),
		Port:            value("DB_PORT", "5433"),
		User:            value("DB_USER", "postgres"),
		Password:        value("DB_PASSWORD", "postgres"),
		Name:            value("DB_NAME", "vozkot"),
		SSLMode:         value("DB_SSLMODE", "disable"),
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxIdle,
		ConnMaxLifetime: maxLifetime,
		ConnMaxIdleTime: maxIdleTime,
	}, nil
}

func value(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func boolean(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func integer(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return parsed, nil
}
