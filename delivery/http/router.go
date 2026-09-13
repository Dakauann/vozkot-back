package http

import (
	"net/http"
	"time"

	httpSwagger "github.com/swaggo/http-swagger"

	authHTTP "vozkot/delivery/http/auth"
	checkoutHTTP "vozkot/delivery/http/checkout"
	eventHTTP "vozkot/delivery/http/event"
	"vozkot/delivery/http/httpx"
	ticketHTTP "vozkot/delivery/http/ticket"
	webhooksHTTP "vozkot/delivery/http/webhooks"
	"vozkot/infra/http/middleware"
)

type AuthMiddleware interface {
	Require(next http.Handler) http.Handler
}

// Middleware is any wrapper the router may apply to a route.
type Middleware interface {
	Require(next http.Handler) http.Handler
}

// Dependencies is what the transport layer needs.
//
// A struct rather than a parameter list: the surface grows with the product,
// and a call site with eight positional arguments is one rename away from
// passing the wrong handler to the wrong route.
type Dependencies struct {
	Auth    *authHTTP.Handler
	Tickets *ticketHTTP.Handler
	// Events is the catalogue: public buyer routes plus operator routes.
	Events   *eventHTTP.Handler
	Checkout *checkoutHTTP.Handler
	// Webhooks is nil when no payment provider is configured, and the route is
	// then not mounted at all — a webhook endpoint that cannot verify a
	// signature must not exist.
	Webhooks       *webhooksHTTP.MercadoPagoHandler
	AuthMiddleware AuthMiddleware
	// CheckoutLimit caps how often one caller may reserve inventory. Nil when
	// Redis is not configured, and then only the per-account limit is absent;
	// CheckoutAdmission still protects the origin as a whole.
	CheckoutLimit Middleware
	// CheckoutAdmission is a per-process bulkhead. It remains active even when
	// Redis or the edge waiting room is unavailable.
	CheckoutAdmission Middleware
	// AuthLimit throttles the unauthenticated credential routes by client
	// address. Nil when Redis is not configured.
	AuthLimit Middleware
	// MediaFiles is the local development asset server, nil when the object
	// store is Cloudflare R2 and the CDN serves the bytes.
	MediaFiles    http.Handler
	AllowedOrigin string
	// Health reports dependencies the load balancer should know about.
	Health func() map[string]any
}

func NewRouter(deps Dependencies) http.Handler {
	router := http.NewServeMux()

	router.HandleFunc("GET /health", func(response http.ResponseWriter, _ *http.Request) {
		payload := map[string]any{"status": "ok", "time": time.Now().UTC()}
		if deps.Health != nil {
			for key, value := range deps.Health() {
				payload[key] = value
			}
		}
		httpx.WriteJSON(response, http.StatusOK, payload)
	})

	// Register, login and refresh are the only routes an anonymous caller can
	// reach that cost real work: a login is a bcrypt compare, and free
	// registration is what makes accounts a renewable resource for anyone
	// holding inventory hostage.
	var throttle func(http.Handler) http.Handler
	if deps.AuthLimit != nil {
		throttle = deps.AuthLimit.Require
	}
	deps.Auth.RegisterPublic(router, throttle)
	deps.Auth.RegisterProtected(router, deps.AuthMiddleware.Require)
	// Passwordless sign-in and the identity block. Mounted only when the
	// encryption keyring exists; the handler decides, because it is the thing
	// that knows whether it was given one.
	deps.Auth.RegisterVerification(router, throttle, deps.AuthMiddleware.Require)

	// Everything under /api/v1 is behind the session. The sub-mux exists
	// because Go's ServeMux applies middleware per pattern, not per prefix.
	// The public catalogue. No session, and the handlers pin the status
	// themselves so a query parameter can never list an unannounced line-up.
	if deps.Events != nil {
		deps.Events.RegisterPublic(router)
	}

	protected := http.NewServeMux()
	deps.Tickets.Register(protected)
	if deps.Events != nil {
		deps.Events.RegisterProtected(protected)
	}
	if deps.Checkout != nil {
		deps.Checkout.Register(protected)
	}
	for _, pattern := range []string{
		"/api/v1/tickets", "/api/v1/tickets/",
		"/api/v1/orders", "/api/v1/orders/",
		"/api/v1/events", "/api/v1/events/",
	} {
		router.Handle(pattern, deps.AuthMiddleware.Require(protected))
	}

	// Checkout carries two extra guards: it reserves inventory, so a script
	// calling it in a loop can hold an event hostage without paying for a single
	// ticket. The account limiter sits inside the session middleware so it can
	// count the authenticated buyer. The admission bulkhead is outermost so an
	// overload is rejected before it can consume an auth/database connection.
	checkout := http.Handler(protected)
	if deps.CheckoutLimit != nil {
		checkout = deps.CheckoutLimit.Require(checkout)
	}
	checkout = deps.AuthMiddleware.Require(checkout)
	if deps.CheckoutAdmission != nil {
		checkout = deps.CheckoutAdmission.Require(checkout)
	}
	router.Handle("/api/v1/checkout", checkout)

	// The webhook is public by necessity — the provider has no session — and
	// authenticated by its HMAC signature instead.
	//
	// Deliberately NOT rate limited, and it must stay that way. Every
	// notification arrives from the provider's own addresses, so a per-address
	// limit would put the entire world's payments in one bucket and start
	// refusing them the moment a sale got busy. What a refusal buys is worse
	// than nothing: a 429 makes Mercado Pago redeliver, which adds load rather
	// than shedding it, and every rejected delivery is a buyer who has paid
	// waiting on the reconciliation sweep instead of on a webhook. The endpoint
	// is already cheap on purpose — one HMAC and one INSERT … ON CONFLICT, no
	// provider call, no order read — and a forgery costs a 401. Pace this at the
	// edge by source address if it ever needs pacing, never here.
	if deps.Webhooks != nil {
		deps.Webhooks.Register(router)
	}

	if deps.MediaFiles != nil {
		router.Handle("GET /media/", http.StripPrefix("/media/", deps.MediaFiles))
	}
	router.Handle("/swagger/", httpSwagger.WrapHandler)

	return middleware.SecurityHeaders(withCORS(withRequestID(router), deps.AllowedOrigin))
}

func withCORS(next http.Handler, allowedOrigin string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if allowedOrigin != "" {
			response.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			response.Header().Set("Access-Control-Allow-Credentials", "true")
			response.Header().Set("Vary", "Origin")
			response.Header().Set("Access-Control-Allow-Headers",
				"Content-Type, Authorization, X-Request-ID, X-Auth-Mode, Idempotency-Key")
			response.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			response.Header().Set("Access-Control-Expose-Headers",
				"Retry-After, X-Checkout-Capacity, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, X-Request-ID")
		}
		if request.Method == http.MethodOptions {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = time.Now().UTC().Format("20060102T150405.000000000")
		}
		response.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(response, request)
	})
}
