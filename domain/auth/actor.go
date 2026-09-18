package auth

import (
	"context"

	"vozkot/domain/user"
)

// Actor is who is making a request.
//
// An authenticated principal reduced to the two facts every authorisation
// decision in this system needs: which account is asking, and whether that
// account is an operator of the platform. Nothing else about a session, the
// email, the token version, the JTI, has ever decided whether a caller may
// read a row, so nothing else is here.
//
// It exists so that a use case can enforce its own rules. Before it, five
// transport packages each declared their own `adminRole = "admin"` and their
// own forbidden error and made the decision next to the HTTP writer, which put
// every authorisation rule in the one layer that a new caller, a CLI, a job,
// another use case, does not go through. Passing this value into a use case
// moves the rule to where the data is, and leaves transport with the two jobs
// it should have: turn a session into an Actor, and turn an error into a status
// code.
type Actor struct {
	ID   string
	Role user.Role
}

// ActorFrom reduces the claims a session carries to an Actor.
//
// A nil or roleless claim produces the ZERO Actor, which owns nothing and is
// not an operator. That default is the reason this is a constructor rather
// than a struct literal at each call site: the failure mode of "no session"
// has to be "no access", and a zero value that happened to satisfy IsAdmin
// would turn a missing middleware into a platform-wide breach.
func ActorFrom(claims *Claims) Actor {
	if claims == nil {
		return Actor{}
	}
	return Actor{ID: claims.UserID, Role: user.Role(claims.Role)}
}

// ActorFromContext is the Actor of the session on this context, if any.
func ActorFromContext(ctx context.Context) Actor {
	claims, _ := ClaimsFromContext(ctx)
	return ActorFrom(claims)
}

// Authenticated reports whether there is an account behind this request.
func (a Actor) Authenticated() bool { return a.ID != "" }

// IsAdmin reports whether this caller operates the platform.
//
// The one place the admin role is compared, so "admin" is spelled once in the
// system and a use case cannot accidentally test for "administrator".
func (a Actor) IsAdmin() bool { return a.Role == user.RoleAdmin }

// Owns reports whether ownerID is this caller's own.
//
// Both empty strings are refused rather than matched. A row with no owner, a
// door sale with no account behind it, must not become everybody's, and an
// unauthenticated Actor must not own the rows that have no owner either.
func (a Actor) Owns(ownerID string) bool {
	return a.ID != "" && ownerID != "" && ownerID == a.ID
}

// MayReach reports whether this caller may act on something owned by ownerID.
//
// The rule every resource in this system shares: an operator may reach
// anything, and everybody else may reach only their own. A use case that needs
// something different, an event's organiser reaching a buyer's refund, say,
// composes it from Owns and IsAdmin rather than restating this.
func (a Actor) MayReach(ownerID string) bool { return a.IsAdmin() || a.Owns(ownerID) }
