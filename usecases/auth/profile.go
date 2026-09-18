package auth

import (
	"context"
	"strings"
	"time"

	"vozkot/domain/user"
)

// Profiles is the identity block: the data a ticket sale legally has to carry.
//
// It is a separate use case from sign-in because it happens at a different
// moment and for a different reason. An account exists as soon as an email is
// proven; the document, the legal name and the date of birth are asked for
// afterwards, once, and only because a purchase cannot lawfully proceed without
// them. Putting them in the sign-in flow would be a form between a person and
// the thing they came for.
type Profiles struct {
	users user.Repository
	now   func() time.Time
}

func NewProfiles(users user.Repository) *Profiles {
	return &Profiles{users: users, now: time.Now}
}

// SaveInput is what the buyer fills in.
type SaveInput struct {
	UserID       string
	DocumentType string
	Document     string
	LegalName    string
	BirthDate    string
	// Gender, City and UF are optional. They are asked for because an organiser
	// needs to know who came to their event, and they are optional because
	// nobody should have to answer that to buy a ticket.
	Gender string
	City   string
	UF     string
}

// Save validates and stores the identity block.
//
// The account's display name is taken from the legal name when it has none,
// which is every account made by a sign-in code. That is the only place the two
// are connected: the display name is a nicety and may later be changed, while
// the legal name is what has to match the document at the door.
func (p *Profiles) Save(ctx context.Context, input SaveInput) (*user.User, error) {
	account, err := p.users.FindByID(ctx, strings.TrimSpace(input.UserID))
	if err != nil {
		return nil, err
	}

	draft := user.ProfileDraft{
		DocumentType: input.DocumentType,
		Document:     input.Document,
		LegalName:    input.LegalName,
		BirthDate:    input.BirthDate,
		Gender:       input.Gender,
		City:         input.City,
		UF:           input.UF,
	}
	// On an account that has already filled the block in, a BLANK identity field
	// means "leave it as it is" rather than "clear it".
	//
	// This is what lets a buyer edit the optional answers, their city, say,
	// without the screen having to send a CPF back. It could not send one: the
	// API returns the document masked and never in full, by design, so a form
	// that had to echo it would either have to be given the real thing or would
	// blank it.
	//
	// It can only ever PRESERVE. A non-blank value still validates in full, so
	// this is not a way to skip a check, and an account with no profile yet
	// falls through to the ordinary rules where every field is required.
	if account.Profile.Complete() {
		carryForward(&draft, account.Profile)
	}

	profile, err := user.NormalizeProfile(draft, p.now())
	if err != nil {
		return nil, err
	}

	// A phone already proven is carried forward. Filling in the identity block
	// a second time, to correct a typo in a name, must not silently
	// un-verify a number.
	profile.Phone = account.Profile.Phone
	profile.PhoneVerifiedAt = account.Profile.PhoneVerifiedAt

	// Completed once. Re-saving updates the values and leaves the original
	// timestamp, because "when did this account become able to buy" is a
	// different question from "when was this last edited".
	if account.Profile.CompletedAt != nil {
		profile.CompletedAt = account.Profile.CompletedAt
	} else {
		completed := p.now().UTC()
		profile.CompletedAt = &completed
	}

	if err := p.users.SaveProfile(ctx, account.ID, profile); err != nil {
		return nil, err
	}

	account.Profile = profile
	if strings.TrimSpace(account.Name) == "" {
		account.Name = profile.LegalName
	}
	return account, nil
}

// carryForward fills blank identity fields from what is already stored.
//
// Only the three that cannot be re-sent by an edit screen. The optional
// demographics are deliberately absent: for those, blank is a real answer:
// somebody clearing their city means they want it cleared, and carrying them
// forward would make the fields impossible to empty once set.
func carryForward(draft *user.ProfileDraft, stored user.Profile) {
	if strings.TrimSpace(draft.Document) == "" {
		draft.Document = stored.Document
		if strings.TrimSpace(draft.DocumentType) == "" {
			draft.DocumentType = string(stored.DocumentType)
		}
	}
	if strings.TrimSpace(draft.LegalName) == "" {
		draft.LegalName = stored.LegalName
	}
	if strings.TrimSpace(draft.BirthDate) == "" {
		draft.BirthDate = stored.BirthDate
	}
}

// Get returns the account with its identity block decrypted.
func (p *Profiles) Get(ctx context.Context, userID string) (*user.User, error) {
	return p.users.FindByID(ctx, strings.TrimSpace(userID))
}
