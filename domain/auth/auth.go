package auth

import "vozkot/domain/user"

type CredentialsInput struct {
	Name       string
	Email      string
	Password   string
	DeviceInfo string
	IPAddress  string
}

type TokenPair struct {
	AccessToken  string
	RefreshToken string
	AccessJTI    string
	User         *user.User
}

type Claims struct {
	UserID       string
	Email        string
	Role         string
	TokenVersion int
	JTI          string
}
