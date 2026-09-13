package main

// The HTTP mode: the same storm, driven through the real router.
//
// A use-case run answers "how fast can PostgreSQL arbitrate inventory". It is
// the right question for the locking design and the wrong one for a capacity
// plan, because it skips everything a real checkout also pays for: TLS, the
// session lookup, the idempotency claim and its completing UPDATE, JSON in and
// out, and — decisively — the admission bulkhead, which caps a replica at
// CHECKOUT_MAX_IN_FLIGHT divided by the latency of the WHOLE request.
//
// So this mode stands up delivery/http.NewRouter with every real dependency
// behind it, gives each buyer their own account and bearer token, and reports
// the number that belongs in a plan: checkouts per second per replica, with the
// bulkhead engaged and its refusals counted rather than hidden.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"gorm.io/gorm"

	delivery "vozkot/delivery/http"
	authHTTP "vozkot/delivery/http/auth"
	checkoutHTTP "vozkot/delivery/http/checkout"
	ticketHTTP "vozkot/delivery/http/ticket"
	authdomain "vozkot/domain/auth"
	idempotencydomain "vozkot/domain/idempotency"
	orderdomain "vozkot/domain/order"
	ticketdomain "vozkot/domain/ticket"
	userdomain "vozkot/domain/user"
	"vozkot/infra/config"
	authMiddleware "vozkot/infra/http/middleware"
	authRepository "vozkot/infra/repositories/auth"
	idempotencyRepository "vozkot/infra/repositories/idempotency"
	mediaRepository "vozkot/infra/repositories/media"
	ticketRepository "vozkot/infra/repositories/ticket"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/security"
	"vozkot/infra/storage"
	authUsecase "vozkot/usecases/auth"
	checkoutUsecase "vozkot/usecases/checkout"
	mediaUsecase "vozkot/usecases/media"
	paymentUsecase "vozkot/usecases/payment"
	ticketUsecase "vozkot/usecases/ticket"
)

// outcome is what one checkout attempt produced, in the vocabulary both modes
// share so the storm loop does not care which one it is running.
type outcome int

const (
	outcomeCreated outcome = iota
	// outcomeSoldOut is the tier running out — expected, and the majority of a
	// well-designed run.
	outcomeSoldOut
	// outcomeShed is the admission bulkhead refusing the request before it
	// could cost a database connection. Only reachable over HTTP, and the whole
	// reason this mode exists.
	outcomeShed
	// outcomeThrottled is a per-account or per-address rate limit.
	outcomeThrottled
	outcomeFailed
)

// fleet is how the storm makes one checkout, whichever mode is running.
type fleet interface {
	// checkout makes one attempt as buyer `buyer`, returning the order id when
	// one was created.
	checkout(ctx context.Context, buyer int, key string, ticketID string, quantity int) (string, outcome, error)
	// close releases whatever the mode stood up.
	close()
}

// ---------------------------------------------------------------------------
// The use-case fleet: what this harness has always measured.

type useCaseFleet struct {
	service *checkoutUsecase.Service
	buyerID string
}

func (f *useCaseFleet) checkout(ctx context.Context, _ int, key, ticketID string, quantity int) (string, outcome, error) {
	item, err := f.service.Start(ctx, checkoutUsecase.StartInput{
		TicketID:       ticketID,
		Quantity:       quantity,
		BuyerID:        f.buyerID,
		BuyerName:      "Buyer " + key,
		BuyerEmail:     "buyer-" + key + "@vozkot.test",
		BuyerDocument:  "12345678909",
		IdempotencyKey: key,
	})
	if err != nil {
		return "", classifyUseCaseError(err), err
	}
	return item.ID, outcomeCreated, nil
}

func (f *useCaseFleet) close() {}

// classifyUseCaseError maps a use-case refusal onto the shared vocabulary.
//
// Sold out and the hold limits land in the same bucket on purpose: over HTTP
// both are a 409, and the two modes have to count the same thing for their
// numbers to be comparable at all.
func classifyUseCaseError(err error) outcome {
	switch {
	case errors.Is(err, ticketdomain.ErrInsufficientStock),
		errors.Is(err, ticketdomain.ErrNotOnSale),
		errors.Is(err, orderdomain.ErrIdempotencyMismatch),
		errors.Is(err, orderdomain.ErrTooManyOpenOrders),
		errors.Is(err, orderdomain.ErrTooManyHeldTickets):
		return outcomeSoldOut
	default:
		return outcomeFailed
	}
}

// ---------------------------------------------------------------------------
// The HTTP fleet: the real router, one account per buyer.

type httpFleet struct {
	server  *httptest.Server
	client  *http.Client
	tokens  []string
	emails  []string
	baseURL string
}

func (f *httpFleet) close() { f.server.Close() }

func (f *httpFleet) checkout(ctx context.Context, buyer int, key, ticketID string, quantity int) (string, outcome, error) {
	body, err := json.Marshal(checkoutHTTP.CheckoutRequest{
		TicketID: ticketID,
		Quantity: quantity,
		Buyer: checkoutHTTP.Buyer{
			Name:     fmt.Sprintf("Buyer %d", buyer),
			Email:    f.emails[buyer],
			Document: "12345678909",
		},
	})
	if err != nil {
		return "", outcomeFailed, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/api/v1/checkout", bytes.NewReader(body))
	if err != nil {
		return "", outcomeFailed, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.tokens[buyer])
	request.Header.Set("Idempotency-Key", key)

	response, err := f.client.Do(request)
	if err != nil {
		return "", outcomeFailed, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))

	switch response.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var envelope checkoutHTTP.OrderEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return "", outcomeFailed, fmt.Errorf("decode 201 body: %w", err)
		}
		return envelope.Data.ID, outcomeCreated, nil
	case http.StatusConflict:
		// Sold out, a reused key, or a hold limit. All are the system saying no
		// on purpose, which is the same shape of answer.
		return "", outcomeSoldOut, nil
	case http.StatusServiceUnavailable:
		return "", outcomeShed, nil
	case http.StatusTooManyRequests:
		return "", outcomeThrottled, nil
	default:
		return "", outcomeFailed, fmt.Errorf("checkout answered %d: %s", response.StatusCode, truncateBody(payload))
	}
}

func truncateBody(payload []byte) string {
	const limit = 200
	if len(payload) > limit {
		return string(payload[:limit]) + "…"
	}
	return string(payload)
}

// newHTTPFleet stands up the real router and an account per buyer.
//
// Every dependency is the production one: the same middleware chain, the same
// idempotency store, the same session guard. The two deliberate departures are
// named in the log, because a harness that silently differs from production is
// worse than no harness — the per-account checkout rate limit is off (thirty a
// minute would refuse a storm that makes hundreds per buyer in seconds, and it
// is not what this run measures), and the hold limits are raised for the same
// reason.
func newHTTPFleet(
	ctx context.Context,
	db *gorm.DB,
	cfg config.Config,
	opts options,
	runID string,
	checkout *checkoutUsecase.Service,
	payments *paymentUsecase.Service,
) (*httpFleet, error) {
	tokens, err := security.NewTokenService(cfg.JWTSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	if err != nil {
		return nil, err
	}
	passwords := security.NewPasswordService(security.MinPasswordHashCost)
	users := userRepository.NewUserRepository(db)
	sessions := authRepository.NewSessionRepository(db)
	keys := idempotencyRepository.NewIdempotencyRepository(db)

	fileStorage, err := storage.New(ctx, cfg.Media)
	if err != nil {
		return nil, err
	}
	ticketService := ticketUsecase.NewService(
		ticketRepository.NewTicketRepository(db),
		mediaUsecase.NewService(mediaRepository.NewMediaRepository(db), fileStorage),
	)

	clients, err := authMiddleware.NewClientIP(config.DefaultTrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	authService := authUsecase.NewService(users, sessions, passwords, tokens, cfg.RefreshTokenTTL)

	router := delivery.NewRouter(delivery.Dependencies{
		Auth:           authHTTP.NewHandler(authService, authHTTP.CookieConfig{}, nil, clients.From),
		Tickets:        ticketHTTP.NewHandler(ticketService),
		Checkout:       checkoutHTTP.NewHandler(checkout, payments, keys, idempotencydomain.DefaultLease),
		AuthMiddleware: authMiddleware.NewAuth(tokens, sessions),
		// The bulkhead is the point of this mode; the per-account limiter is
		// not, and would refuse the storm before the bulkhead ever saw it.
		CheckoutAdmission: authMiddleware.NewConcurrencyLimit(opts.maxInFlight),
		AllowedOrigin:     "http://localhost:3000",
	})

	server := newHTTP2Server(router)
	client := server.Client()
	client.Timeout = 30 * time.Second
	if transport, ok := client.Transport.(*http.Transport); ok {
		// Without this, hundreds of concurrent buyers churn TCP connections and
		// the harness measures the handshake instead of the checkout.
		transport.MaxIdleConns = opts.buyers * 2
		transport.MaxIdleConnsPerHost = opts.buyers * 2
		transport.MaxConnsPerHost = opts.buyers * 2
	}

	fleet := &httpFleet{
		server:  server,
		client:  client,
		tokens:  make([]string, opts.buyers),
		emails:  make([]string, opts.buyers),
		baseURL: server.URL,
	}

	// One account per buyer, with a real session row, so the guard's lookup and
	// the per-buyer hold lock are both genuinely exercised.
	now := time.Now().UTC()
	for index := 0; index < opts.buyers; index++ {
		account := &userdomain.User{
			ID:           fmt.Sprintf("usr_http_%s_%d", runID, index),
			Name:         fmt.Sprintf("Buyer %d", index),
			Email:        fmt.Sprintf("buyer-%s-%d@vozkot.test", runID, index),
			PasswordHash: "x",
			Role:         userdomain.RoleUser,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := users.Create(ctx, account); err != nil {
			return nil, fmt.Errorf("seed buyer %d: %w", index, err)
		}
		pair, err := tokens.Issue(account)
		if err != nil {
			return nil, err
		}
		_, refreshHash, err := tokens.GenerateRefreshToken()
		if err != nil {
			return nil, err
		}
		session := &authdomain.Session{
			ID:               fmt.Sprintf("sess_http_%s_%d", runID, index),
			UserID:           account.ID,
			RefreshTokenHash: refreshHash,
			AccessJTI:        pair.AccessJTI,
			CreatedAt:        now,
			ExpiresAt:        now.Add(cfg.RefreshTokenTTL),
		}
		if err := sessions.Create(ctx, session); err != nil {
			return nil, fmt.Errorf("seed session %d: %w", index, err)
		}
		fleet.tokens[index] = pair.AccessToken
		fleet.emails[index] = account.Email
	}

	return fleet, nil
}

// cleanupHTTPAccounts removes the accounts and sessions this mode seeded.
// Sessions first: they reference the users.
func cleanupHTTPAccounts(db *gorm.DB, runID string) {
	db.Exec("DELETE FROM sessions WHERE id LIKE ?", "sess_http_"+runID+"_%")
	db.Exec("DELETE FROM orders WHERE buyer_id LIKE ?", "usr_http_"+runID+"_%")
	db.Exec("DELETE FROM users WHERE id LIKE ?", "usr_http_"+runID+"_%")
}
