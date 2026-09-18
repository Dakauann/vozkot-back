// Package asaastest is Asaas at the HTTP boundary, including the parts that
// refuse you.
//
// It exists because the load harness was stubbing the WRONG PROVIDER. The
// harness carried its own fake Mercado Pago while `PAYMENT_PROVIDER=asaas`, so
// a storm exercised an adapter production does not use, against a fake with no
// limits, and reported a clean audit. Everything that actually bounds an
// on-sale here, the per-endpoint buckets and the account quota, was invisible
// to the one tool built to find it.
//
// A real package rather than a helper in a _test.go file, because Go will not
// let a command import test files, which is why the harness grew its own fake
// instead of reusing the one that already existed. Same reason net/http has
// httptest.
//
// # The limits are the point
//
// Measured from live response headers on 2026-09-18 and recorded in
// asaas/budget_test.go:
//
//	GET /payments      (list and search)  140 per 60s
//	GET /payments/{id} (read one charge)  100 per 60s
//	account quota                         25,000 per 12 hours, all endpoints
//
// Every charge this system creates spends one GET /payments on its idempotency
// search, so 140/min is a ceiling of about 2.3 orders per second however many
// replicas are running. A 5,000 ticket on-sale therefore needs some 36 minutes
// to issue its PIX codes, against a 30 minute hold. That arithmetic is the
// whole reason this package models refusals instead of just succeeding quickly:
// a harness that never gets throttled cannot show it.
//
// Refusals carry RateLimit-Reset, and a caller that keeps pushing gets the 403
// the real account returns once it has been asked to stop and did not.
package asaastest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"vozkot/infra/asaas"
)

// Live limits, as the provider published them.
const (
	SearchLimit   = 140
	SearchWindow  = time.Minute
	ReadLimit     = 100
	ReadWindow    = time.Minute
	AccountLimit  = 25_000
	AccountWindow = 12 * time.Hour
	// BlockAfter is how many refusals in a row earn a 403 instead of a 429.
	//
	// The real account escalated exactly this way: a burst of 429s, then
	// "Seu acesso foi temporariamente bloqueado por exceder o limite de
	// requisições". A client that respects RateLimit-Reset never reaches it,
	// which makes this the assertion that the pacing works.
	BlockAfter = 50
)

// envelope is Asaas's list shape. Declared here because the type in the asaas
// package is unexported, and the wire format is what this stub owes callers.
type envelope[T any] struct {
	Data []T `json:"data"`
}

// Provider is a fake Asaas account.
//
// Safe for concurrent use: a load harness hits it from every worker at once,
// which is the only interesting way to hit it.
type Provider struct {
	mu       sync.Mutex
	payments map[string]*asaas.Payment
	requests []string
	created  []asaas.Payment

	customerID string
	nextID     int
	qrPayload  string

	// The buckets. Timestamps rather than counters, so a window slides the way
	// the provider's does instead of resetting on a tick.
	searchHits  []time.Time
	readHits    []time.Time
	accountHits []time.Time
	refusals    int
	blocked     bool

	// Counters a caller can read afterwards.
	throttled int
	forbidden int

	// limits off is how the adapter's own unit tests use this: they assert the
	// wire format, and a bucket would make them flaky.
	limits bool
	now    func() time.Time
}

// New returns a provider that enforces the real limits.
func New() *Provider {
	return &Provider{
		payments:   map[string]*asaas.Payment{},
		customerID: "cus_000001",
		qrPayload:  "00020126-ASAAS-PIX-PAYLOAD",
		limits:     true,
		now:        time.Now,
	}
}

// Unlimited returns a provider that never refuses, for tests about the wire
// format rather than about capacity.
func Unlimited() *Provider {
	p := New()
	p.limits = false
	return p
}

// WithClock replaces the clock, so a test can walk a window forward without
// sleeping through it.
func (p *Provider) WithClock(clock func() time.Time) *Provider {
	p.now = clock
	return p
}

// Server starts the stub. The caller closes it.
func (p *Provider) Server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(p.handle))
}

// Stats reports what the run cost, which is the number a capacity plan wants.
func (p *Provider) Stats() (requests, throttled, forbidden int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests), p.throttled, p.forbidden
}

// Calls counts recorded requests whose "METHOD /path?query" starts with prefix.
func (p *Provider) Calls(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, call := range p.requests {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

// spend records one hit against a sliding window and reports the seconds left
// when the window is full.
func (p *Provider) spend(hits *[]time.Time, limit int, window time.Duration) (int, bool) {
	now := p.now()
	cutoff := now.Add(-window)
	kept := (*hits)[:0]
	for _, at := range *hits {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	*hits = kept
	if len(*hits) >= limit {
		// Seconds until the oldest hit leaves the window, which is what
		// RateLimit-Reset means.
		wait := int((*hits)[0].Add(window).Sub(now).Seconds()) + 1
		if wait < 1 {
			wait = 1
		}
		return wait, false
	}
	*hits = append(*hits, now)
	return 0, true
}

// refuse writes the provider's own refusal, with the header a paced client
// reads.
func (p *Provider) refuse(response http.ResponseWriter, reset int) {
	p.refusals++
	if p.refusals > BlockAfter {
		p.blocked = true
	}
	if p.blocked {
		p.forbidden++
		response.WriteHeader(http.StatusForbidden)
		_, _ = response.Write([]byte(`{"errors":[{"code":"access_blocked",` +
			`"description":"Seu acesso foi temporariamente bloqueado por exceder o limite de requisicoes. ` +
			`Tente novamente dentro de alguns minutos."}]}`))
		return
	}
	p.throttled++
	response.Header().Set("RateLimit-Reset", strconv.Itoa(reset))
	response.WriteHeader(http.StatusTooManyRequests)
	_, _ = response.Write([]byte(`{"errors":[{"code":"rate_limit",` +
		`"description":"Seu acesso foi temporariamente bloqueado por exceder o limite de requisicoes. ` +
		`Tente novamente dentro de alguns minutos."}]}`))
}

func (p *Provider) handle(response http.ResponseWriter, request *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, request.Method+" "+request.URL.Path+"?"+request.URL.RawQuery)
	response.Header().Set("Content-Type", "application/json")
	path := request.URL.Path

	if p.limits {
		// The account quota first: it spans every endpoint, so exceeding it
		// refuses reads and writes alike.
		if reset, ok := p.spend(&p.accountHits, AccountLimit, AccountWindow); !ok {
			p.refuse(response, reset)
			return
		}
		switch {
		case request.Method == http.MethodGet && path == "/payments":
			if reset, ok := p.spend(&p.searchHits, SearchLimit, SearchWindow); !ok {
				p.refuse(response, reset)
				return
			}
		case request.Method == http.MethodGet && strings.HasPrefix(path, "/payments/") &&
			!strings.HasSuffix(path, "/pixQrCode"):
			if reset, ok := p.spend(&p.readHits, ReadLimit, ReadWindow); !ok {
				p.refuse(response, reset)
				return
			}
		}
		// A request that got through is the client behaving, so the escalation
		// counter starts over.
		p.refusals = 0
	}

	switch {
	case request.Method == http.MethodGet && path == "/customers":
		_ = json.NewEncoder(response).Encode(envelope[asaas.Customer]{
			Data: []asaas.Customer{{
				ID:       p.customerID,
				Document: request.URL.Query().Get("cpfCnpj"),
			}},
		})

	case request.Method == http.MethodPost && path == "/customers":
		_ = json.NewEncoder(response).Encode(asaas.Customer{ID: p.customerID})

	case request.Method == http.MethodGet && path == "/payments":
		reference := request.URL.Query().Get("externalReference")
		out := envelope[asaas.Payment]{}
		for _, item := range p.payments {
			if item.ExternalReference == reference {
				out.Data = append(out.Data, *item)
			}
		}
		_ = json.NewEncoder(response).Encode(out)

	case request.Method == http.MethodPost && path == "/payments":
		var draft asaas.Payment
		_ = json.NewDecoder(request.Body).Decode(&draft)
		p.created = append(p.created, draft)
		p.nextID++
		draft.ID = fmt.Sprintf("pay_%06d", p.nextID)
		draft.Status = asaas.StatusPending
		p.payments[draft.ID] = &draft
		_ = json.NewEncoder(response).Encode(draft)

	case request.Method == http.MethodGet && strings.HasSuffix(path, "/pixQrCode"):
		_ = json.NewEncoder(response).Encode(asaas.PixQRCode{
			EncodedImage: "aGVsbG8=",
			Payload:      p.qrPayload,
		})

	case request.Method == http.MethodGet && strings.HasPrefix(path, "/payments/"):
		id := strings.TrimPrefix(path, "/payments/")
		item, found := p.payments[id]
		if !found {
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"errors":[{"description":"not found"}]}`))
			return
		}
		_ = json.NewEncoder(response).Encode(item)

	case request.Method == http.MethodPost && strings.HasSuffix(path, "/refund"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/payments/"), "/refund")
		if item, found := p.payments[id]; found {
			item.Status = asaas.StatusRefunded
		}
		_, _ = response.Write([]byte(`{}`))

	case request.Method == http.MethodDelete && strings.HasPrefix(path, "/payments/"):
		id := strings.TrimPrefix(path, "/payments/")
		if item, found := p.payments[id]; found {
			item.Deleted = true
		}
		_, _ = response.Write([]byte(`{"deleted":true}`))

	default:
		response.WriteHeader(http.StatusNotFound)
	}
}

// Approve moves a charge to received, the way a buyer paying does.
func (p *Provider) Approve(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, found := p.payments[id]
	if !found {
		return false
	}
	item.Status = asaas.StatusReceived
	return true
}

// IDs lists the charges created so far, oldest first.
func (p *Provider) IDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, p.nextID)
	for index := 1; index <= p.nextID; index++ {
		out = append(out, fmt.Sprintf("pay_%06d", index))
	}
	return out
}
