package user

import (
	"context"
	"testing"
	"time"

	domain "vozkot/domain/user"
	"vozkot/infra/testsupport"
)

// The demographics have to survive a round trip through encryption, and they
// have to be UNREADABLE in the column.
//
// Both halves are the point. The first is ordinary persistence; the second is
// the claim the privacy note in domain/user makes, and a claim about encryption
// that nothing checks is a claim that quietly stops being true the first time
// somebody changes a column type.
func TestProfileDemographicsRoundTripSealed(t *testing.T) {
	testsupport.Encryption(t)
	db := testsupport.Database(t)
	repository := NewUserRepository(db)
	ctx := context.Background()

	account := &domain.User{
		ID:    testsupport.Unique("usr"),
		Name:  "Maria Souza",
		Email: testsupport.Unique("maria") + "@exemplo.com.br",
		Role:  domain.RoleUser,
	}
	if err := repository.Create(ctx, account); err != nil {
		t.Fatalf("create user: %v", err)
	}

	profile, err := domain.NormalizeProfile(domain.ProfileDraft{
		DocumentType: "cpf",
		Document:     freshCPF(t),
		LegalName:    "Maria Souza",
		BirthDate:    "1994-03-21",
		Gender:       "female",
		City:         "  Natal ",
		UF:           "rn",
	}, time.Now())
	if err != nil {
		t.Fatalf("normalize profile: %v", err)
	}
	completed := time.Now().UTC()
	profile.CompletedAt = &completed

	if err := repository.SaveProfile(ctx, account.ID, profile); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	stored, err := repository.FindByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Profile.Gender != domain.GenderFemale {
		t.Errorf("gender = %q, want %q", stored.Profile.Gender, domain.GenderFemale)
	}
	if stored.Profile.City != "Natal" {
		t.Errorf("city = %q, want %q (trimmed)", stored.Profile.City, "Natal")
	}
	if stored.Profile.UF != "RN" {
		t.Errorf("uf = %q, want %q (upper-cased)", stored.Profile.UF, "RN")
	}

	// Read the raw bytes. The plaintext must not be in the column.
	var raw struct {
		Gender []byte
		City   []byte
		UF     []byte
	}
	if err := db.Raw(`SELECT gender, city, uf FROM users WHERE id = ?`, account.ID).Scan(&raw).Error; err != nil {
		t.Fatalf("read raw columns: %v", err)
	}
	for name, value := range map[string][]byte{"gender": raw.Gender, "city": raw.City, "uf": raw.UF} {
		if len(value) == 0 {
			t.Fatalf("%s was stored empty; the round trip above cannot have been real", name)
		}
	}
	if string(raw.City) == "Natal" {
		t.Fatal("the city is in the clear in the database")
	}
	if string(raw.Gender) == "female" {
		t.Fatal("the gender is in the clear in the database")
	}
}

// Leaving the optional fields blank must be accepted, and must come back blank
// rather than as some default. A buyer who declines to answer is a buyer, not a
// validation failure.
func TestProfileDemographicsAreOptional(t *testing.T) {
	testsupport.Encryption(t)
	db := testsupport.Database(t)
	repository := NewUserRepository(db)
	ctx := context.Background()

	account := &domain.User{
		ID:    testsupport.Unique("usr"),
		Name:  "João Silva",
		Email: testsupport.Unique("joao") + "@exemplo.com.br",
		Role:  domain.RoleUser,
	}
	if err := repository.Create(ctx, account); err != nil {
		t.Fatalf("create user: %v", err)
	}

	profile, err := domain.NormalizeProfile(domain.ProfileDraft{
		DocumentType: "cpf",
		Document:     freshCPF(t),
		LegalName:    "João Silva",
		BirthDate:    "1990-01-15",
	}, time.Now())
	if err != nil {
		t.Fatalf("a profile with no demographics was refused: %v", err)
	}
	completed := time.Now().UTC()
	profile.CompletedAt = &completed
	if err := repository.SaveProfile(ctx, account.ID, profile); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	stored, err := repository.FindByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Profile.Gender != "" || stored.Profile.City != "" || stored.Profile.UF != "" {
		t.Fatalf("blank demographics came back as %q/%q/%q",
			stored.Profile.Gender, stored.Profile.City, stored.Profile.UF)
	}
	// Still able to buy: the optional block must not gate completeness.
	if !stored.Profile.Complete() {
		t.Fatal("a profile without the optional demographics was not considered complete")
	}
}

// freshCPF builds a CPF nothing in the database is using yet.
//
// A fixed number cannot work here: the blind index on the document is UNIQUE
// across accounts: it is what stops one CPF being spread over several of them
// to get around the per-document purchase caps, and these tests share a
// database that keeps its rows between runs. A literal would pass once and then
// collide with the account the previous run created.
//
// The first nine digits come from the shared counter, so two tests in one run
// cannot collide either; the last two are the real check digits, because
// NormalizeProfile verifies them.
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
