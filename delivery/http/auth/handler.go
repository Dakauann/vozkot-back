package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	domain "vozkot/domain/auth"
	"vozkot/domain/cache"
	"vozkot/domain/user"
	usecase "vozkot/usecases/auth"
)

type CookieConfig struct {
	Domain        string
	Secure        bool
	AccessMaxAge  time.Duration
	RefreshMaxAge time.Duration
}

// AccountLimiter caps attempts against ONE account, whatever address they come
// from.
//
// The per-address limit the router applies cannot do this: a guessing run
// spread across a few thousand hosts trips no address counter, and the account
// being guessed never knows. The account is in the request body, which only
// this handler has parsed, so this half of the limit lives here.
type AccountLimiter interface {
	Allow(ctx context.Context, key string) (cache.Decision, bool)
	WriteRefusal(response http.ResponseWriter, decision cache.Decision)
}

type Handler struct {
	service *usecase.Service
	cookies CookieConfig
	// accounts limits login attempts per email. Nil when Redis is absent, and
	// then only this half of the limit is gone; the per-address one remains.
	accounts AccountLimiter
	// callerIP resolves the address recorded on a session. Supplied rather than
	// computed here so one policy about which forwarding headers may be
	// believed covers the whole application; nil falls back to the connecting
	// address, which trusts nothing.
	callerIP func(*http.Request) string
	// verification and profiles drive the passwordless routes. Both nil means
	// those routes are simply not mounted, which is what a deployment with no
	// mail provider and no encryption keys gets.
	verification *usecase.Verification
	profiles     *usecase.Profiles
}

// WithVerification adds the passwordless and profile routes.
//
// A setter rather than two more constructor parameters, because every existing
// caller, the tests, the load harness, wants the handler without them, and
// widening the constructor would make each of those state that it does not.
func (h *Handler) WithVerification(verification *usecase.Verification, profiles *usecase.Profiles) *Handler {
	h.verification = verification
	h.profiles = profiles
	return h
}

func NewHandler(
	service *usecase.Service,
	cookies CookieConfig,
	accounts AccountLimiter,
	callerIP func(*http.Request) string,
) *Handler {
	handler := &Handler{service: service, cookies: cookies, callerIP: callerIP}
	// A nil interface holding a nil pointer is not nil, and would panic on the
	// first login. Only a genuinely supplied limiter is kept.
	if accounts != nil && !reflect.ValueOf(accounts).IsNil() {
		handler.accounts = accounts
	}
	if handler.callerIP == nil {
		handler.callerIP = peerAddress
	}
	return handler
}

// RegisterPublic mounts the unauthenticated credential routes.
//
// throttle is applied to all three. They are the only routes an anonymous
// caller can reach that cost real work; a login is a bcrypt compare at cost
// twelve, about a quarter second of CPU, so an unthrottled flood is both a
// credential-stuffing surface and a way to spend the fleet's CPU without
// holding an account. A nil throttle mounts them bare, which is what happens
// when Redis is not configured.
func (h *Handler) RegisterPublic(router *http.ServeMux, throttle func(http.Handler) http.Handler) {
	if throttle == nil {
		throttle = func(next http.Handler) http.Handler { return next }
	}
	router.Handle("POST /auth/login", throttle(http.HandlerFunc(h.login)))
	router.Handle("POST /auth/refresh", throttle(http.HandlerFunc(h.refresh)))
}

func (h *Handler) RegisterProtected(router *http.ServeMux, require func(http.Handler) http.Handler) {
	router.Handle("POST /auth/logout", require(http.HandlerFunc(h.logout)))
	router.Handle("GET /user/me", require(http.HandlerFunc(h.me)))
}

// @Summary		Entrar com email e senha
// @Description	Autentica as credenciais e abre uma sessão. Envie X-Auth-Mode: cookie para receber cookies httpOnly em vez dos tokens no corpo.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Param		request body LoginRequest true "Credenciais"
// @Success		200 {object} AuthResponse
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Router		/auth/login [post]
func (h *Handler) login(response http.ResponseWriter, request *http.Request) {
	var body LoginRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}

	// Counted before the password is verified, so a guessing run pays the
	// limiter rather than a bcrypt compare per attempt. Normalised the same way
	// the lookup normalises it, or "Maria@..." and "maria@..." would be two
	// budgets for one account.
	if h.accounts != nil {
		email := strings.ToLower(strings.TrimSpace(body.Email))
		if decision, allowed := h.accounts.Allow(request.Context(), "email:"+email); !allowed {
			h.accounts.WriteRefusal(response, decision)
			return
		}
	}

	pair, err := h.service.Login(request.Context(), domain.CredentialsInput{
		Email: body.Email, Password: body.Password,
		IPAddress: h.callerIP(request), DeviceInfo: request.UserAgent(),
	})
	if err != nil {
		httpx.WriteError(response, authStatus(err), err)
		return
	}
	h.writeAuth(response, request, http.StatusOK, pair)
}

// @Summary		Renovar a sessão
// @Description	Rotaciona o token de atualização de uso único e emite um novo token de acesso. O refresh token pode vir no corpo ou no cookie httpOnly refreshToken.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Param		request body RefreshTokenRequest false "Token de atualização"
// @Success		200 {object} AuthResponse
// @Failure		401 {object} ErrorResponse
// @Router		/auth/refresh [post]
func (h *Handler) refresh(response http.ResponseWriter, request *http.Request) {
	var body RefreshTokenRequest
	_ = httpx.ReadJSON(response, request, &body)
	if body.RefreshToken == "" {
		if cookie, err := request.Cookie("refreshToken"); err == nil {
			body.RefreshToken = cookie.Value
		}
	}
	if body.RefreshToken == "" {
		httpx.WriteError(response, http.StatusUnauthorized, domain.ErrUnauthorized)
		return
	}
	pair, err := h.service.Refresh(request.Context(), body.RefreshToken, h.callerIP(request), request.UserAgent())
	if err != nil {
		h.clearCookies(response)
		httpx.WriteError(response, http.StatusUnauthorized, err)
		return
	}
	h.writeAuth(response, request, http.StatusOK, pair)
}

// @Summary		Consultar o usuário atual
// @Description	Retorna a identidade ligada à sessão autenticada.
// @Tags			Autenticação
// @Produce		json
// @Security		BearerAuth
// @Success		200 {object} UserResponse
// @Failure		401 {object} ErrorResponse
// @Router		/user/me [get]
func (h *Handler) me(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	item, err := h.service.Me(request.Context(), claims)
	if err != nil {
		httpx.WriteError(response, http.StatusUnauthorized, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, userResponse(item))
}

// @Summary		Encerrar a sessão atual
// @Description	Revoga a sessão atual e remove os cookies de autenticação.
// @Tags			Autenticação
// @Produce		json
// @Security		BearerAuth
// @Success		200 {object} MessageResponse
// @Failure		401 {object} ErrorResponse
// @Router		/auth/logout [post]
func (h *Handler) logout(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	if err := h.service.Logout(request.Context(), claims); err != nil {
		httpx.WriteError(response, http.StatusUnauthorized, err)
		return
	}
	h.clearCookies(response)
	httpx.WriteJSON(response, http.StatusOK, MessageResponse{Message: "Sessão encerrada com sucesso"})
}

func (h *Handler) writeAuth(response http.ResponseWriter, request *http.Request, status int, pair *domain.TokenPair) {
	payload := AuthResponse{AccessToken: pair.AccessToken, RefreshToken: pair.RefreshToken, TokenType: "Bearer", User: userResponse(pair.User)}
	if cookieMode(request) {
		h.setCookies(response, pair)
		payload.AccessToken = ""
		payload.RefreshToken = ""
	}
	httpx.WriteJSON(response, status, payload)
}

func (h *Handler) setCookies(response http.ResponseWriter, pair *domain.TokenPair) {
	http.SetCookie(response, &http.Cookie{Name: "accessToken", Value: pair.AccessToken, Path: "/", Domain: h.cookies.Domain, HttpOnly: true, Secure: h.cookies.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(h.cookies.AccessMaxAge.Seconds())})
	http.SetCookie(response, &http.Cookie{Name: "refreshToken", Value: pair.RefreshToken, Path: "/auth/refresh", Domain: h.cookies.Domain, HttpOnly: true, Secure: h.cookies.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(h.cookies.RefreshMaxAge.Seconds())})
	identity, _ := json.Marshal(userResponse(pair.User))
	encodedIdentity := strings.ReplaceAll(url.QueryEscape(string(identity)), "+", "%20")
	http.SetCookie(response, &http.Cookie{Name: "userData", Value: encodedIdentity, Path: "/", Domain: h.cookies.Domain, Secure: h.cookies.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(h.cookies.RefreshMaxAge.Seconds())})
}

func (h *Handler) clearCookies(response http.ResponseWriter) {
	for _, cookie := range []struct{ name, path string }{{"accessToken", "/"}, {"refreshToken", "/auth/refresh"}, {"userData", "/"}} {
		http.SetCookie(response, &http.Cookie{Name: cookie.name, Value: "", Path: cookie.path, Domain: h.cookies.Domain, HttpOnly: cookie.name != "userData", Secure: h.cookies.Secure, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	}
}

func cookieMode(request *http.Request) bool {
	return strings.EqualFold(request.Header.Get("X-Auth-Mode"), "cookie")
}

func userResponse(item *user.User) UserResponse {
	return UserResponse{ID: item.ID, Name: item.Name, Email: item.Email, Role: string(item.Role)}
}

// peerAddress is the fallback when no resolver was supplied: the address the
// connection actually came from.
//
// It deliberately does NOT read X-Forwarded-For. That header is client-supplied
// text, and believing it without knowing the request came through a trusted
// proxy lets any caller write whatever address they like onto a session record,
// and, where the same value keys a rate limit, choose their own bucket.
func peerAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}

func authStatus(err error) int {
	switch {
	case errors.Is(err, domain.ErrInvalidCredentials), errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, user.ErrEmailAlreadyExists):
		return http.StatusConflict
	case errors.Is(err, domain.ErrWeakPassword):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadRequest
	}
}
