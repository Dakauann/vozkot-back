package auth

type LoginRequest struct {
	Email    string `json:"email" example:"agente@empresa.com.br"`
	Password string `json:"password" example:"SenhaForte1"`
}

type RegisterRequest struct {
	Name     string `json:"name" example:"Maria Silva"`
	Email    string `json:"email" example:"maria@empresa.com.br"`
	Password string `json:"password" example:"SenhaForte1"`
}

type RefreshTokenRequest struct {
	RefreshToken string `json:"refreshToken,omitempty" example:"token_de_atualizacao"`
}

type UserResponse struct {
	ID    string `json:"id" example:"usr_a1b2c3"`
	Name  string `json:"name" example:"Maria Silva"`
	Email string `json:"email" example:"maria@empresa.com.br"`
	Role  string `json:"role" example:"user"`
}

type AuthResponse struct {
	AccessToken  string       `json:"accessToken,omitempty" example:"eyJhbGciOi..."`
	RefreshToken string       `json:"refreshToken,omitempty" example:"token_de_atualizacao"`
	TokenType    string       `json:"tokenType" example:"Bearer"`
	User         UserResponse `json:"user"`
}

type MessageResponse struct {
	Message string `json:"message" example:"Sessão encerrada com sucesso"`
}

type ErrorResponse struct {
	Error string `json:"error" example:"credenciais inválidas"`
}
