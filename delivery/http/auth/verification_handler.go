package auth

import (
	"errors"
	"net/http"
	"time"

	"vozkot/delivery/http/httpx"
	domain "vozkot/domain/auth"
	"vozkot/domain/user"
	usecase "vozkot/usecases/auth"
)

// The passwordless routes.
//
// Two of them are anonymous and both cost real work; one sends an email, the
// other runs a slow hash, so both sit behind the same throttle the password
// routes use. That throttle is per address; the per-DESTINATION limits live in
// the use case, because an address limit does nothing against a script spread
// over a few thousand hosts all aimed at one person's inbox.

// StartRequest asks for a code.
type StartRequest struct {
	Email string `json:"email" example:"maria@exemplo.com.br"`
}

// StartResponse is what comes back, and it says nothing about whether the
// address has an account. That symmetry is the point: any difference here;
// a status code, a field, a response time, turns this endpoint into a way to
// ask who has an account on a ticketing site.
type StartResponse struct {
	ChallengeID string    `json:"challengeId" example:"vch_9f2c1d8a"`
	ExpiresAt   time.Time `json:"expiresAt"`
	// ResendAt is when another code may be requested, so the screen can show a
	// real countdown instead of guessing at the server's rule.
	ResendAt time.Time `json:"resendAt"`
}

// VerifyRequest answers one.
type VerifyRequest struct {
	ChallengeID string `json:"challengeId" example:"vch_9f2c1d8a"`
	Code        string `json:"code" example:"481905"`
}

// PhoneStartRequest begins confirming a number on an account that exists.
type PhoneStartRequest struct {
	Phone string `json:"phone" example:"(84) 99440-9624"`
}

// ProfileRequest is the legally required identity block.
type ProfileRequest struct {
	DocumentType string `json:"documentType" enums:"cpf,cnpj,passport" example:"cpf"`
	Document     string `json:"document" example:"529.982.247-25"`
	LegalName    string `json:"legalName" example:"Maria Souza"`
	BirthDate    string `json:"birthDate" example:"1994-03-21"`
}

// ProfileResponse reports the state of the identity block.
//
// It carries a MASKED document and never the number. The client's only real
// question is "is this filled in, and does it look like the right document";
// showing the digits back would put them in a response body, a browser cache
// and a screenshot for no gain.
type ProfileResponse struct {
	Complete      bool   `json:"complete" example:"true"`
	DocumentType  string `json:"documentType,omitempty" example:"cpf"`
	DocumentMask  string `json:"documentMask,omitempty" example:"529.***.***-25"`
	LegalName     string `json:"legalName,omitempty" example:"Maria Souza"`
	BirthDate     string `json:"birthDate,omitempty" example:"1994-03-21"`
	PhoneMask     string `json:"phoneMask,omitempty" example:"(84) *****-9624"`
	PhoneVerified bool   `json:"phoneVerified" example:"false"`
}

// RegisterVerification mounts the passwordless routes.
func (h *Handler) RegisterVerification(
	router *http.ServeMux,
	throttle func(http.Handler) http.Handler,
	require func(http.Handler) http.Handler,
) {
	if h.verification == nil {
		return
	}
	if throttle == nil {
		throttle = func(next http.Handler) http.Handler { return next }
	}
	router.Handle("POST /auth/email/start", throttle(http.HandlerFunc(h.startEmailSignIn)))
	router.Handle("POST /auth/email/verify", throttle(http.HandlerFunc(h.verifyEmailSignIn)))

	// A missing throttle mounts the anonymous routes bare, because bare is what
	// they are without Redis and refusing to serve them would take the whole
	// sign-in down with the cache. A missing `require` is the opposite: these
	// routes read and write somebody's identity block, and mounting them
	// unguarded would be worse than not mounting them at all. So they are not
	// mounted. (It used to dereference the nil instead, which turned a wiring
	// mistake into a panic inside a request.)
	if require == nil {
		return
	}
	router.Handle("POST /auth/phone/start", require(http.HandlerFunc(h.startPhone)))
	router.Handle("POST /auth/phone/verify", require(http.HandlerFunc(h.verifyPhone)))
	router.Handle("GET /user/profile", require(http.HandlerFunc(h.getProfile)))
	router.Handle("PUT /user/profile", require(http.HandlerFunc(h.saveProfile)))
	router.Handle("PUT /user/password", require(http.HandlerFunc(h.setPassword)))
}

// SetPasswordRequest adds a password to an account, or changes it.
type SetPasswordRequest struct {
	// Current is required only when the account already has a password.
	Current string `json:"current,omitempty"`
	New     string `json:"new" example:"UmaSenhaForte1"`
}

// @Summary		Definir ou trocar a senha
// @Description	Adiciona uma senha à conta, como SEGUNDA forma de entrar: a primeira continua sendo o código por e-mail, e a conta funciona sem senha. Definir a primeira senha exige apenas a sessão; trocar uma senha existente exige a senha atual, porque uma sessão emprestada não deve bastar para trancar o dono fora da própria conta.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body SetPasswordRequest true "Senha"
// @Success		204
// @Failure		401 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse "Senha fraca, ou senha atual ausente"
// @Router		/user/password [put]
func (h *Handler) setPassword(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, domain.ErrUnauthorized)
		return
	}
	var body SetPasswordRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	err := h.service.SetPassword(request.Context(), usecase.SetPasswordInput{
		UserID:  claims.UserID,
		Current: body.Current,
		New:     body.New,
	})
	if err != nil {
		httpx.WriteCodedError(response, passwordStatus(err), passwordCode(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func passwordStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusNoContent
	case errors.Is(err, domain.ErrInvalidCredentials):
		return http.StatusUnauthorized
	case errors.Is(err, domain.ErrWeakPassword),
		errors.Is(err, usecase.ErrPasswordRequired),
		errors.Is(err, usecase.ErrCurrentPasswordRequired):
		return http.StatusUnprocessableEntity
	case errors.Is(err, user.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func passwordCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrWeakPassword):
		return CodeWeakPassword
	case errors.Is(err, usecase.ErrCurrentPasswordRequired):
		return CodeCurrentPasswordRequired
	default:
		return ""
	}
}

// @Summary		Pedir um código de acesso por e-mail
// @Description	Envia um código de seis dígitos para o e-mail informado. A resposta é idêntica quer o e-mail já tenha conta ou não: de propósito, para que este endpoint não possa ser usado para descobrir quem tem conta. O código vale 10 minutos, serve uma vez só e aceita no máximo 5 tentativas.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Param		request body StartRequest true "E-mail"
// @Success		202 {object} StartResponse
// @Failure		400 {object} ErrorResponse
// @Failure		429 {object} ErrorResponse "Código pedido cedo demais, ou vezes demais"
// @Router		/auth/email/start [post]
func (h *Handler) startEmailSignIn(response http.ResponseWriter, request *http.Request) {
	var body StartRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	started, err := h.verification.StartEmailSignIn(request.Context(), body.Email)
	if err != nil {
		httpx.WriteCodedError(response, verificationStatus(err), verificationCode(err), err)
		return
	}
	// 202: the code is on its way, and nothing about the account is asserted.
	httpx.WriteJSON(response, http.StatusAccepted, StartResponse(started))
}

// @Summary		Entrar com o código recebido por e-mail
// @Description	Confere o código e abre uma sessão. Se o e-mail ainda não tinha conta, ela é criada agora: entrar e cadastrar são a mesma chamada, porque separá-las exigiria revelar antes se o e-mail já existe.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Param		request body VerifyRequest true "Código"
// @Success		200 {object} AuthResponse
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		410 {object} ErrorResponse "O código expirou ou já foi usado"
// @Failure		429 {object} ErrorResponse "Tentativas demais"
// @Router		/auth/email/verify [post]
func (h *Handler) verifyEmailSignIn(response http.ResponseWriter, request *http.Request) {
	var body VerifyRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	result, err := h.verification.VerifyEmailSignIn(
		request.Context(), body.ChallengeID, body.Code,
		h.callerIP(request), request.UserAgent(),
	)
	if err != nil {
		httpx.WriteCodedError(response, verificationStatus(err), verificationCode(err), err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	h.writeAuth(response, request, status, result.Tokens)
}

// @Summary		Pedir um código para confirmar o celular
// @Description	Envia um código para o número informado. Responde 503 enquanto não houver provedor de SMS configurado: o código nunca é registrado em log nem devolvido na resposta. O restante do fluxo, limite de envios, limite de tentativas, expiração, é real.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body PhoneStartRequest true "Celular"
// @Success		202 {object} StartResponse
// @Failure		401 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Failure		429 {object} ErrorResponse
// @Router		/auth/phone/start [post]
func (h *Handler) startPhone(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, domain.ErrUnauthorized)
		return
	}
	var body PhoneStartRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	started, err := h.verification.StartPhoneVerification(request.Context(), claims.UserID, body.Phone)
	if err != nil {
		httpx.WriteCodedError(response, verificationStatus(err), verificationCode(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusAccepted, StartResponse(started))
}

// @Summary		Confirmar o celular com o código
// @Description	Confere o código e marca o número como confirmado na conta.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body VerifyRequest true "Código"
// @Success		200 {object} ProfileResponse
// @Failure		401 {object} ErrorResponse
// @Failure		410 {object} ErrorResponse
// @Failure		429 {object} ErrorResponse
// @Router		/auth/phone/verify [post]
func (h *Handler) verifyPhone(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, domain.ErrUnauthorized)
		return
	}
	var body VerifyRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	account, err := h.verification.VerifyPhone(request.Context(), claims.UserID, body.ChallengeID, body.Code)
	if err != nil {
		httpx.WriteCodedError(response, verificationStatus(err), verificationCode(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, profileResponse(account))
}

// @Summary		Consultar os dados cadastrais
// @Description	Retorna o estado do cadastro obrigatório. O documento vem MASCARADO: o cliente só precisa saber se está preenchido e se parece o documento certo.
// @Tags			Autenticação
// @Produce		json
// @Security		BearerAuth
// @Success		200 {object} ProfileResponse
// @Failure		401 {object} ErrorResponse
// @Router		/user/profile [get]
func (h *Handler) getProfile(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, domain.ErrUnauthorized)
		return
	}
	account, err := h.profiles.Get(request.Context(), claims.UserID)
	if err != nil {
		httpx.WriteError(response, authStatus(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, profileResponse(account))
}

// @Summary		Salvar os dados cadastrais
// @Description	Grava documento, nome completo e data de nascimento. Exigidos por lei para vender ingresso no Brasil, meia-entrada é limitada por CPF e a cobrança PIX não é emitida sem documento. Todos os campos são armazenados criptografados.
// @Tags			Autenticação
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body ProfileRequest true "Dados cadastrais"
// @Success		200 {object} ProfileResponse
// @Failure		401 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "O documento já pertence a outra conta"
// @Failure		422 {object} ErrorResponse
// @Router		/user/profile [put]
func (h *Handler) saveProfile(response http.ResponseWriter, request *http.Request) {
	claims, _ := domain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, domain.ErrUnauthorized)
		return
	}
	var body ProfileRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	account, err := h.profiles.Save(request.Context(), usecase.SaveInput{
		UserID:       claims.UserID,
		DocumentType: body.DocumentType,
		Document:     body.Document,
		LegalName:    body.LegalName,
		BirthDate:    body.BirthDate,
	})
	if err != nil {
		httpx.WriteCodedError(response, profileStatus(err), profileCode(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, profileResponse(account))
}

func profileResponse(account *user.User) ProfileResponse {
	profile := account.Profile
	return ProfileResponse{
		Complete:      profile.Complete(),
		DocumentType:  string(profile.DocumentType),
		DocumentMask:  maskDocument(profile.Document),
		LegalName:     profile.LegalName,
		BirthDate:     profile.BirthDate,
		PhoneMask:     maskPhone(profile.Phone),
		PhoneVerified: profile.PhoneVerified(),
	}
}

// maskDocument shows enough to recognise a document and not enough to use one.
//
// First three and last two, which is the convention every Brazilian service
// uses for a CPF and what a person actually checks against their own card.
func maskDocument(document string) string {
	runes := []rune(document)
	if len(runes) < 6 {
		if len(runes) == 0 {
			return ""
		}
		return "***"
	}
	return string(runes[:3]) + "***" + string(runes[len(runes)-2:])
}

// maskPhone keeps the last four, the way a bank's confirmation screen does.
func maskPhone(phone string) string {
	runes := []rune(phone)
	if len(runes) < 6 {
		if len(runes) == 0 {
			return ""
		}
		return "***"
	}
	return "***" + string(runes[len(runes)-4:])
}

// verificationStatus maps a code failure onto the status that describes it.
//
// 410 for expired and used, rather than 400: the request was well formed and
// the thing it names is genuinely gone. 429 for a spent attempt budget, because
// the answer is "later", not "wrong".
func verificationStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, domain.ErrInvalidContact),
		errors.Is(err, user.ErrInvalidPhone):
		return http.StatusUnprocessableEntity
	case errors.Is(err, domain.ErrChallengeNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrChallengeExpired), errors.Is(err, domain.ErrChallengeUsed):
		return http.StatusGone
	case errors.Is(err, domain.ErrTooManyAttempts),
		errors.Is(err, domain.ErrResendTooSoon),
		errors.Is(err, domain.ErrTooManyRequests):
		return http.StatusTooManyRequests
	case errors.Is(err, domain.ErrInvalidCode), errors.Is(err, domain.ErrInvalidCredentials):
		return http.StatusUnauthorized
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, domain.ErrDeliveryUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Machine-readable names for the refusals a screen has to react to
// differently: "wrong code, try again" and "ask for a new one" are two
// different screens, and the message that distinguishes them ships in four
// languages.
const (
	CodeInvalid          = "code_invalid"
	CodeExpired          = "code_expired"
	CodeAlreadyUsed      = "code_used"
	CodeTooManyAttempts  = "too_many_attempts"
	CodeResendTooSoon    = "resend_too_soon"
	CodeTooManyRequests  = "too_many_requests"
	CodeChallengeUnknown = "challenge_unknown"
	CodeContactInvalid   = "contact_invalid"
	CodeDocumentInUse    = "document_in_use"
	CodeProfileInvalid   = "profile_invalid"
	CodeUnderage         = "underage"
	// CodeWeakPassword and CodeCurrentPasswordRequired let the password form
	// point at the field that is wrong rather than showing one message above
	// both of them.
	// CodeDeliveryUnavailable means the channel is not configured. The screen
	// offers the other way in rather than a retry that cannot succeed.
	CodeDeliveryUnavailable     = "delivery_unavailable"
	CodeWeakPassword            = "weak_password"
	CodeCurrentPasswordRequired = "current_password_required"
)

func verificationCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrInvalidCode):
		return CodeInvalid
	case errors.Is(err, domain.ErrChallengeExpired):
		return CodeExpired
	case errors.Is(err, domain.ErrChallengeUsed):
		return CodeAlreadyUsed
	case errors.Is(err, domain.ErrTooManyAttempts):
		return CodeTooManyAttempts
	case errors.Is(err, domain.ErrResendTooSoon):
		return CodeResendTooSoon
	case errors.Is(err, domain.ErrTooManyRequests):
		return CodeTooManyRequests
	case errors.Is(err, domain.ErrChallengeNotFound):
		return CodeChallengeUnknown
	case errors.Is(err, domain.ErrInvalidContact), errors.Is(err, user.ErrInvalidPhone):
		return CodeContactInvalid
	case errors.Is(err, domain.ErrDeliveryUnavailable):
		return CodeDeliveryUnavailable
	default:
		return ""
	}
}

func profileStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, user.ErrDocumentInUse):
		return http.StatusConflict
	case errors.Is(err, user.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, user.ErrInvalidDocumentType),
		errors.Is(err, user.ErrInvalidDocument),
		errors.Is(err, user.ErrInvalidLegalName),
		errors.Is(err, user.ErrInvalidBirthDate),
		errors.Is(err, user.ErrUnderage),
		errors.Is(err, user.ErrInvalidPhone):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func profileCode(err error) string {
	switch {
	case errors.Is(err, user.ErrDocumentInUse):
		return CodeDocumentInUse
	case errors.Is(err, user.ErrUnderage):
		return CodeUnderage
	case errors.Is(err, user.ErrInvalidDocumentType),
		errors.Is(err, user.ErrInvalidDocument),
		errors.Is(err, user.ErrInvalidLegalName),
		errors.Is(err, user.ErrInvalidBirthDate):
		return CodeProfileInvalid
	default:
		return ""
	}
}
