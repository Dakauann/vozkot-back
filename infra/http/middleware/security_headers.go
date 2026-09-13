package middleware

import "net/http"

// SecurityHeaders sets the response headers every JSON API should carry.
//
// None of them protects the API from a caller; they protect a BROWSER from a
// response. An API that one day serves an HTML error page, or whose JSON is
// opened directly in a tab, is one MIME-sniff or one frame away from being a
// vector, and these headers close both at no cost.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		headers := response.Header()
		// Never guess a content type from the bytes.
		headers.Set("X-Content-Type-Options", "nosniff")
		// The API is not a page and must not be framed by one.
		headers.Set("X-Frame-Options", "DENY")
		// Nothing here needs to load anything; the strictest policy costs
		// nothing on JSON and blunts any injected markup.
		headers.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		// A payment reference must not leak into a third party's logs through
		// the Referer of whatever the client navigates to next.
		headers.Set("Referrer-Policy", "no-referrer")
		// Responses carry sessions and orders; a shared cache must not keep
		// them.
		if headers.Get("Cache-Control") == "" {
			headers.Set("Cache-Control", "no-store")
		}
		if request.TLS != nil {
			// Only meaningful over TLS, and only set there so a plain-HTTP
			// development server is not asked to pin itself to HTTPS.
			headers.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(response, request)
	})
}
