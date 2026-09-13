package auth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	domain "vozkot/domain/auth"
	"vozkot/domain/user"
	usecase "vozkot/usecases/auth"
)

type CookieConfig struct {
	Domain        string
	Secure        bool
	AccessMaxAge  time.Duration
	RefreshMaxAge time.Duration
}

type Handler struct {
	service *usecase.Service
	cookies CookieConfig
}

func NewHandler(service *usecase.Service, cookies CookieConfig) *Handler {
	return &Handler{service: service, cookies: cookies}
}

func (h *Handler) RegisterPublic(router *http.ServeMux) {
	router.HandleFunc("POST /auth/register", h.register)
	router.HandleFunc("POST /auth/login", h.login)
	router.HandleFunc("POST /auth/refresh", h.refresh)
}

func (h *Handler) RegisterProtected(router *http.ServeMux, require func(http.Handler) http.Handler) {
	router.Handle("POST /auth/logout", require(http.HandlerFunc(h.logout)))
	router.Handle("GET /user/me", require(http.HandlerFunc(h.me)))
}

// @Summary		Cadastrar uma conta
// @Description	Cria um usuário, inicia uma sessão e envia os tokens em cookies httpOnly no modo navegador. A senha deve ter ao menos 8 caracteres, maiúscula, minúscula e número.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Param		request body RegisterRequest true "Dados do cadastro"
// @Success		201 {object} AuthResponse
// @Failure		400 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/auth/register [post]
func (h *Handler) register(response http.ResponseWriter, request *http.Request) {
	var body RegisterRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	pair, err := h.service.Register(request.Context(), domain.CredentialsInput{
		Name: body.Name, Email: body.Email, Password: body.Password,
		IPAddress: clientIP(request), DeviceInfo: request.UserAgent(),
	})
	if err != nil {
		httpx.WriteError(response, authStatus(err), err)
		return
	}
	h.writeAuth(response, request, http.StatusCreated, pair)
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
	pair, err := h.service.Login(request.Context(), domain.CredentialsInput{
		Email: body.Email, Password: body.Password,
		IPAddress: clientIP(request), DeviceInfo: request.UserAgent(),
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
	pair, err := h.service.Refresh(request.Context(), body.RefreshToken, clientIP(request), request.UserAgent())
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

func clientIP(request *http.Request) string {
	if forwarded := strings.Split(request.Header.Get("X-Forwarded-For"), ",")[0]; strings.TrimSpace(forwarded) != "" {
		return strings.TrimSpace(forwarded)
	}
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
