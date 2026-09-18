package auth

import (
	"context"
	"testing"
	"time"

	"vozkot/domain/user"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/testsupport"
)

// Editing the optional answers must not disturb the identity block.
//
// This is the test that guards the "blank means leave it" rule. The settings
// screen cannot send a CPF back: the API returns it masked and never in full,
// so it sends blanks, and if those blanks ever started CLEARING the document
// instead of preserving it, every buyer who edited their city would lose the
// document their ticket is checked against at the door.
func TestSavingOnlyTheOptionalAnswersKeepsTheIdentityBlock(t *testing.T) {
	testsupport.Encryption(t)
	db := testsupport.Database(t)
	users := userRepository.NewUserRepository(db)
	profiles := NewProfiles(users)
	ctx := context.Background()

	email := testsupport.Unique("maria") + "@exemplo.com.br"
	cleanupUser(t, email)
	account := seedPasswordAccount(t, email, "")

	document := freshCPF(t)
	saved, err := profiles.Save(ctx, SaveInput{
		UserID:       account.ID,
		DocumentType: "cpf",
		Document:     document,
		LegalName:    "Maria Souza",
		BirthDate:    "1994-03-21",
		Gender:       "female",
		City:         "Natal",
		UF:           "RN",
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if !saved.Profile.Complete() {
		t.Fatal("the profile was not marked complete")
	}
	completedAt := saved.Profile.CompletedAt

	// Now the settings screen: only the optional answers, everything else blank.
	edited, err := profiles.Save(ctx, SaveInput{
		UserID: account.ID,
		Gender: "non_binary",
		City:   "Recife",
		UF:     "PE",
	})
	if err != nil {
		t.Fatalf("editing the optional answers: %v", err)
	}

	if edited.Profile.Document != document {
		t.Fatalf("document = %q, want it untouched (%q)", edited.Profile.Document, document)
	}
	if edited.Profile.DocumentType != user.DocumentCPF {
		t.Fatalf("document type = %q, want cpf", edited.Profile.DocumentType)
	}
	if edited.Profile.LegalName != "Maria Souza" {
		t.Fatalf("legal name = %q, want it untouched", edited.Profile.LegalName)
	}
	if edited.Profile.BirthDate != "1994-03-21" {
		t.Fatalf("birth date = %q, want it untouched", edited.Profile.BirthDate)
	}
	if edited.Profile.Gender != user.GenderNonBinary {
		t.Fatalf("gender = %q, want non_binary", edited.Profile.Gender)
	}
	if edited.Profile.City != "Recife" || edited.Profile.UF != "PE" {
		t.Fatalf("city/uf = %q/%q, want Recife/PE", edited.Profile.City, edited.Profile.UF)
	}
	// "When did this account become able to buy" is a different question from
	// "when was this last edited", so the original stamp survives.
	//
	// Compared at MICROSECOND precision, which is what PostgreSQL stores. The
	// first save returns the value Go built, with nanoseconds; the second reads
	// it back through the column, which has already rounded it. A fresh stamp
	// would differ by milliseconds and still fails this, which is the thing
	// actually being guarded.
	if edited.Profile.CompletedAt == nil {
		t.Fatal("completedAt was cleared by an edit")
	}
	if got, want := edited.Profile.CompletedAt.Truncate(time.Microsecond).UTC(),
		completedAt.Truncate(time.Microsecond).UTC(); !got.Equal(want) {
		t.Fatalf("completedAt moved on an edit: %v -> %v", want, got)
	}
}

// Clearing an optional answer must actually clear it. Carrying these forward
// the way the identity fields are carried would make them impossible to empty
// once set, which is the opposite of what "optional" means.
func TestAnOptionalAnswerCanBeCleared(t *testing.T) {
	testsupport.Encryption(t)
	db := testsupport.Database(t)
	profiles := NewProfiles(userRepository.NewUserRepository(db))
	ctx := context.Background()

	email := testsupport.Unique("joao") + "@exemplo.com.br"
	cleanupUser(t, email)
	account := seedPasswordAccount(t, email, "")

	if _, err := profiles.Save(ctx, SaveInput{
		UserID:       account.ID,
		DocumentType: "cpf",
		Document:     freshCPF(t),
		LegalName:    "João Silva",
		BirthDate:    "1990-01-15",
		Gender:       "male",
		City:         "Olinda",
		UF:           "PE",
	}); err != nil {
		t.Fatalf("first save: %v", err)
	}

	cleared, err := profiles.Save(ctx, SaveInput{UserID: account.ID})
	if err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if cleared.Profile.Gender != "" || cleared.Profile.City != "" || cleared.Profile.UF != "" {
		t.Fatalf("the optional answers survived being cleared: %q/%q/%q",
			cleared.Profile.Gender, cleared.Profile.City, cleared.Profile.UF)
	}
	if cleared.Profile.Document == "" {
		t.Fatal("clearing the optional answers also cleared the document")
	}
}

// An account with NO profile yet still has to supply everything: the
// preserve-on-blank rule must not become a way to skip the required block.
func TestABlankFirstSaveIsStillRefused(t *testing.T) {
	testsupport.Encryption(t)
	db := testsupport.Database(t)
	profiles := NewProfiles(userRepository.NewUserRepository(db))

	email := testsupport.Unique("ana") + "@exemplo.com.br"
	cleanupUser(t, email)
	account := seedPasswordAccount(t, email, "")

	if _, err := profiles.Save(context.Background(), SaveInput{
		UserID: account.ID,
		City:   "Recife",
	}); err == nil {
		t.Fatal("an account with no profile completed one by sending only a city")
	}
}

// freshCPF builds a CPF nothing in the database is using yet. The blind index
// on the document is unique across accounts and these tests share a database
// that keeps its rows, so a literal would pass once and collide forever after.
func freshCPF(t *testing.T) string {
	t.Helper()
	unique := testsupport.Unique("")
	digits := make([]byte, 0, 9)
	for index := len(unique) - 1; index >= 0 && len(digits) < 9; index-- {
		if unique[index] >= '0' && unique[index] <= '9' {
			digits = append(digits, unique[index])
		}
	}
	for len(digits) < 9 {
		digits = append(digits, '1')
	}

	value := make([]int, 9)
	for index := range value {
		value[index] = int(digits[index] - '0')
	}
	for _, position := range []int{9, 10} {
		sum := 0
		for index := 0; index < position; index++ {
			sum += value[index] * (position + 1 - index)
		}
		check := (sum * 10) % 11
		if check == 10 {
			check = 0
		}
		value = append(value, check)
	}

	out := make([]byte, 0, 11)
	for _, digit := range value {
		out = append(out, byte('0'+digit))
	}
	return string(out)
}
