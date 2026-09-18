package http

import (
	"net/http"
	"time"

	httpSwagger "github.com/swaggo/http-swagger"

	admissionHTTP "vozkot/delivery/http/admission"
	authHTTP "vozkot/delivery/http/auth"
	checkoutHTTP "vozkot/delivery/http/checkout"
	eventHTTP "vozkot/delivery/http/event"
	"vozkot/delivery/http/httpx"
	payoutHTTP "vozkot/delivery/http/payout"
	refundHTTP "vozkot/delivery/http/refund"
	reportHTTP "vozkot/delivery/http/report"
	seatingHTTP "vozkot/delivery/http/seating"
	ticketHTTP "vozkot/delivery/http/ticket"
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
// WebhookRegistrar mounts one provider's callback route.
//
// An interface rather than a concrete handler so the router does not name a
// payment provider. Which one is mounted is decided once, in the container,
// from PAYMENT_PROVIDER, and this file stays true whichever it is.
type WebhookRegistrar interface {
	Register(router *http.ServeMux)
}

type Dependencies struct {
	Auth    *authHTTP.Handler
	Tickets *ticketHTTP.Handler
	// Events is the catalogue: public buyer routes plus operator routes.
	Events   *eventHTTP.Handler
	Checkout *checkoutHTTP.Handler
	// Refunds is the cancellation surface: the buyer's request and the
	// organiser's inbox. Nil leaves both unmounted, which is what a deployment
	// with no payment provider gets: a refund endpoint that cannot refund is
	// worse than no endpoint.
	Refunds *refundHTTP.Handler
	// Payouts is the organiser's balance and extract. Read-only.
	Payouts *payoutHTTP.Handler
	// Reports is the organiser's audience dashboard and the attendee export.
	Reports *reportHTTP.Handler
	// Admissions is the door: scanning a code at an event, and a holder's own
	// tickets under their order.
	Admissions *admissionHTTP.Handler
	// Seating is reserved seating: the organiser's room editor, and the public
	// seat map a buyer picks from. Nil leaves both unmounted, which is what a
	// deployment selling only general admission gets.
	Seating *seatingHTTP.Handler
	// Webhooks is nil when no payment provider is configured, and the route is
	// then not mounted at all; a webhook endpoint that cannot verify a
	// signature must not exist.
	Webhooks       WebhookRegistrar
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
	// Metrics counts and times every request. Nil leaves the API unmeasured,
	// which is what a deployment with no monitoring gets; it is never a reason
	// to refuse to serve.
	Metrics Middleware
	// MediaFiles is the local development asset server, nil when the object
	// store is Cloudflare R2 and the CDN serves the bytes.
	MediaFiles    http.Handler
	AllowedOrigin string
	// Health reports dependencies the load balancer should know about.
	Health func() map[string]any
}

// ProtectedPrefixes is where the authenticated mux is actually mounted.
//
// THE ALLOW-LIST IS THE MOUNT, and that is the trap. A handler registering a
// path on the protected mux is not reachable until its prefix appears here: the
// route exists on a mux nothing routes to, so it answers 404 rather than 401
// and reads exactly like a missing handler. It shipped that way once, for
// /api/v1/organiser/.
//
// A package-level var rather than a literal inside NewRouter so the invariant
// is testable. See TestEveryProtectedRouteHasAMount. Adding a route under a
// NEW prefix means adding the prefix here.
var ProtectedPrefixes = []string{
	"/api/v1/tickets", "/api/v1/tickets/",
	"/api/v1/orders", "/api/v1/orders/",
	"/api/v1/events", "/api/v1/events/",
	"/api/v1/refund-requests", "/api/v1/refund-requests/",
	// The organiser's own account: balance, statement and portfolio report.
	// Every one is scoped to the session rather than to a path parameter,
	// which is why they share a prefix of their own.
	"/api/v1/organiser", "/api/v1/organiser/",
	"/api/v1/venues", "/api/v1/venues/",
	"/api/v1/layouts", "/api/v1/layouts/",
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
	if deps.Seating != nil {
		// The seat map is public for the same reason the catalogue is: it is
		// what somebody looks at before they have an account. The view it
		// serves carries no order id and no hold deadline, so being public
		// tells a visitor which chairs are free and nothing about who has
		// the others.
		deps.Seating.RegisterPublicRoutes(router)
	}

	protected := http.NewServeMux()
	deps.Tickets.Register(protected)
	if deps.Events != nil {
		deps.Events.RegisterProtected(protected)
	}
	if deps.Checkout != nil {
		deps.Checkout.Register(protected)
	}
	if deps.Refunds != nil {
		// Split across two prefixes on purpose: the two routes that act on an
		// order live under /api/v1/orders/, and the inbox has its own prefix
		// because it is a listing of requests rather than of orders.
		deps.Refunds.RegisterOrderRoutes(protected)
		deps.Refunds.Register(protected)
	}
	if deps.Payouts != nil {
		// The organiser's own money, under /api/v1/organiser/. Behind the
		// session, and scoped to the caller inside the handler rather than by
		// a path parameter, so there is nothing to tamper with to read
		// somebody else's balance.
		deps.Payouts.Register(protected)
	}
	if deps.Admissions != nil {
		// Split for the same reason refunds are: the door lives under the
		// event it guards, and a holder's tickets under the order that paid
		// for them.
		deps.Admissions.RegisterOrderRoutes(protected)
		deps.Admissions.Register(protected)
	}
	if deps.Reports != nil {
		// Under /api/v1/events/{id}/, beside the operator routes the events
		// handler already mounts there.
		deps.Reports.Register(protected)
	}
	if deps.Seating != nil {
		// The editor: venues, layouts and putting a room on sale. Behind the
		// session, and every route authorized in usecases/seating against the
		// actor's ownership rather than here.
		deps.Seating.Register(protected)
	}
	for _, pattern := range ProtectedPrefixes {
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

	// The webhook is public by necessity, the provider has no session, and
	// authenticated by its HMAC signature instead.
	//
	// Deliberately NOT rate limited, and it must stay that way. Every
	// notification arrives from the provider's own addresses, so a per-address
	// limit would put the entire world's payments in one bucket and start
	// refusing them the moment a sale got busy. What a refusal buys is worse
	// than nothing: a 429 makes Mercado Pago redeliver, which adds load rather
	// than shedding it, and every rejected delivery is a buyer who has paid
	// waiting on the reconciliation sweep instead of on a webhook. The endpoint
	// is already cheap on purpose: one HMAC and one INSERT … ON CONFLICT, no
	// provider call, no order read, and a forgery costs a 401. Pace this at the
	// edge by source address if it ever needs pacing, never here.
	if deps.Webhooks != nil {
		deps.Webhooks.Register(router)
	}

	if deps.MediaFiles != nil {
		router.Handle("GET /media/", http.StripPrefix("/media/", deps.MediaFiles))
	}
	router.Handle("/swagger/", httpSwagger.WrapHandler)

	// Metrics sits INNERMOST, around the mux and nothing else.
	//
	// Inside withCORS so a preflight is not counted as a request the API
	// served: an OPTIONS is answered and returned before it ever reaches a
	// handler, and counting it would inflate the request rate with traffic no
	// buyer made. Inside withRequestID for the same reason in reverse: the
	// duration recorded should be the work, not the wrapper.
	var handler http.Handler = router
	if deps.Metrics != nil {
		handler = deps.Metrics.Require(handler)
	}
	return middleware.SecurityHeaders(withCORS(withRequestID(handler), deps.AllowedOrigin))
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
