package database_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	authDomain "vozkot/domain/auth"
	mediaDomain "vozkot/domain/media"
	ticketDomain "vozkot/domain/ticket"
	userDomain "vozkot/domain/user"
	"vozkot/infra/config"
	"vozkot/infra/database"
	"vozkot/infra/database/schema"
	authRepository "vozkot/infra/repositories/auth"
	mediaRepository "vozkot/infra/repositories/media"
	ticketRepository "vozkot/infra/repositories/ticket"
	userRepository "vozkot/infra/repositories/user"
)

func TestPostgresRepositories(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	migrationDB, err := database.NewMigrationDatabase(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("connect migration database: %v", err)
	}
	migrationSQL, err := migrationDB.DB()
	if err != nil {
		t.Fatalf("access migration connection: %v", err)
	}
	defer migrationSQL.Close()
	if err := database.RunMigrations(ctx, migrationDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, model := range []any{&schema.User{}, &schema.Session{}, &schema.Ticket{}, &schema.Media{}} {
		if !migrationDB.Migrator().HasTable(model) {
			t.Fatalf("migration did not create table for %T", model)
		}
	}

	db, err := database.NewApplicationDatabase(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("connect application database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("access application pool: %v", err)
	}
	defer sqlDB.Close()

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	users := userRepository.NewUserRepository(tx)
	sessions := authRepository.NewSessionRepository(tx)
	tickets := ticketRepository.NewTicketRepository(tx)
	gallery := mediaRepository.NewMediaRepository(tx)

	createdUser := &userDomain.User{
		ID:           "usr_" + suffix,
		Name:         "Postgres Integration",
		Email:        "postgres-" + suffix + "@vozkot.local",
		PasswordHash: "$2a$12$integration-placeholder",
		Role:         userDomain.RoleUser,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := users.Create(ctx, createdUser); err != nil {
		t.Fatalf("create user: %v", err)
	}
	loadedUser, err := users.FindByEmail(ctx, strings.ToUpper(createdUser.Email))
	if err != nil || loadedUser.ID != createdUser.ID {
		t.Fatalf("find user by normalized email: user=%v err=%v", loadedUser, err)
	}

	currentHash := strings.Repeat("a", 64)
	nextHash := strings.Repeat("b", 64)
	createdSession := &authDomain.Session{
		ID:               "sess_" + suffix,
		UserID:           createdUser.ID,
		RefreshTokenHash: currentHash,
		AccessJTI:        "jti-initial-" + suffix,
		DeviceInfo:       "integration-test",
		IPAddress:        "127.0.0.1",
		ExpiresAt:        now.Add(time.Hour),
		CreatedAt:        now,
	}
	if err := sessions.Create(ctx, createdSession); err != nil {
		t.Fatalf("create session: %v", err)
	}
	rotated, err := sessions.Rotate(ctx, createdSession.ID, currentHash, nextHash, "jti-next-"+suffix, now.Add(time.Second))
	if err != nil || !rotated {
		t.Fatalf("rotate session: rotated=%t err=%v", rotated, err)
	}
	previous, err := sessions.FindByPreviousRefreshTokenHash(ctx, currentHash)
	if err != nil || previous.RefreshTokenHash != nextHash {
		t.Fatalf("find rotated session: session=%v err=%v", previous, err)
	}
	if err := sessions.Revoke(ctx, createdSession.ID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}
	if _, err := sessions.FindByAccessJTI(ctx, createdUser.ID, "jti-next-"+suffix); !errors.Is(err, authDomain.ErrSessionNotFound) {
		t.Fatalf("revoked session remained active: %v", err)
	}

	createdTicket := &ticketDomain.Ticket{
		ID:          "tkt_" + suffix,
		OwnerID:     createdUser.ID,
		EventName:   "Festival Aurora " + suffix,
		Title:       "Pista Premium",
		Description: "Repository integration test",
		Venue:       "Arena Castelao",
		City:        "Fortaleza, CE",
		StartsAt:    now.Add(720 * time.Hour),
		PriceCents:  24000,
		Currency:    ticketDomain.DefaultCurrency,
		Quantity:    500,
		Status:      ticketDomain.StatusDraft,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := tickets.Create(ctx, createdTicket); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	createdTicket.Status = ticketDomain.StatusOnSale
	createdTicket.PriceCents = 26000
	createdTicket.UpdatedAt = now.Add(time.Minute)
	if err := tickets.Update(ctx, createdTicket); err != nil {
		t.Fatalf("update ticket: %v", err)
	}
	loadedTicket, err := tickets.GetByID(ctx, createdTicket.ID)
	if err != nil || loadedTicket.Status != ticketDomain.StatusOnSale || loadedTicket.PriceCents != 26000 {
		t.Fatalf("get updated ticket: ticket=%v err=%v", loadedTicket, err)
	}
	filtered, err := tickets.List(ctx, ticketDomain.Filter{Status: ticketDomain.StatusOnSale, Query: "aurora " + suffix})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("filter tickets: count=%d err=%v", len(filtered), err)
	}
	total, err := tickets.Count(ctx, ticketDomain.Filter{Query: "aurora " + suffix})
	if err != nil || total != 1 {
		t.Fatalf("count tickets: total=%d err=%v", total, err)
	}

	position, err := gallery.NextPosition(ctx, createdTicket.ID)
	if err != nil || position != 0 {
		t.Fatalf("first gallery position: position=%d err=%v", position, err)
	}
	createdMedia := &mediaDomain.Media{
		ID:          "med_" + suffix,
		TicketID:    createdTicket.ID,
		Kind:        mediaDomain.KindImage,
		StorageKey:  "tickets/" + createdTicket.ID + "/med_" + suffix + ".jpg",
		URL:         "https://cdn.vozkot.local/tickets/" + createdTicket.ID + "/med_" + suffix + ".jpg",
		ContentType: "image/jpeg",
		SizeBytes:   284133,
		Position:    position,
		CreatedAt:   now,
	}
	if err := gallery.Create(ctx, createdMedia); err != nil {
		t.Fatalf("create media: %v", err)
	}
	galleries, err := gallery.ListByTicketIDs(ctx, []string{createdTicket.ID})
	if err != nil || len(galleries[createdTicket.ID]) != 1 {
		t.Fatalf("list galleries: galleries=%v err=%v", galleries, err)
	}
	if next, err := gallery.NextPosition(ctx, createdTicket.ID); err != nil || next != 1 {
		t.Fatalf("next gallery position: position=%d err=%v", next, err)
	}

	// Deleting the ticket must take its media with it: the foreign key, not the
	// application, is what guarantees no gallery outlives its listing.
	if err := tickets.Delete(ctx, createdTicket.ID); err != nil {
		t.Fatalf("delete ticket: %v", err)
	}
	if _, err := gallery.GetByID(ctx, createdMedia.ID); !errors.Is(err, mediaDomain.ErrNotFound) {
		t.Fatalf("media outlived its ticket: %v", err)
	}

	duplicate := *createdUser
	duplicate.ID = "dup_" + suffix
	if err := users.Create(ctx, &duplicate); !errors.Is(err, userDomain.ErrEmailAlreadyExists) {
		t.Fatalf("duplicate email error = %v", err)
	}
}
