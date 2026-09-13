// Command seed creates the baseline accounts and the development catalogue a
// fresh installation needs. It is idempotent, so existing records are reported
// and left untouched instead of being overwritten.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"vozkot/domain/auth"
	"vozkot/domain/user"
	"vozkot/infra/config"
	"vozkot/infra/database"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/security"
)

type account struct {
	Name     string
	Email    string
	Password string
	Role     user.Role
}

func main() {
	loadEnv()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}

	// The catalogue sends its covers through the same image pipeline and object
	// storage as the dashboard. Local disk is quick, while a remote R2 bucket can
	// take a few minutes for all 160 uploads, so this command needs a wider bound
	// than API startup.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	migrationDB, err := database.NewMigrationDatabase(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("connect for migrations: %v", err)
	}
	migrationSQL, err := migrationDB.DB()
	if err != nil {
		log.Fatalf("access migration connection: %v", err)
	}
	if err := database.RunMigrations(ctx, migrationDB); err != nil {
		migrationSQL.Close()
		log.Fatalf("run migrations: %v", err)
	}
	migrationSQL.Close()

	db, err := database.NewApplicationDatabase(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	databaseSQL, err := db.DB()
	if err != nil {
		log.Fatalf("access connection pool: %v", err)
	}
	defer databaseSQL.Close()

	users := userRepository.NewUserRepository(db)
	passwords := security.NewPasswordService(security.MinPasswordHashCost)

	seedAccounts := accounts()
	for _, item := range seedAccounts {
		created, err := seed(ctx, users, passwords, item)
		if err != nil {
			log.Fatalf("seed %s: %v", item.Email, err)
		}
		if created {
			log.Printf("created %s account: %s", item.Role, item.Email)
			continue
		}
		log.Printf("skipped %s account: %s already exists", item.Role, item.Email)
	}

	ownerEmail := strings.ToLower(strings.TrimSpace(seedAccounts[1].Email))
	owner, err := users.FindByEmail(ctx, ownerEmail)
	if err != nil {
		log.Fatalf("load mock event owner %s: %v", ownerEmail, err)
	}
	summary, err := seedMockEvents(ctx, db, cfg, owner.ID, time.Now())
	if err != nil {
		log.Fatalf("seed mock events: %v", err)
	}
	log.Printf(
		"mock catalogue ready: %d events created, %d already present, %d covers attached, %d already present, %d ticket tiers created, %d updated, %d already present",
		summary.EventsCreated,
		summary.EventsSkipped,
		summary.CoversAttached,
		summary.CoversSkipped,
		summary.TiersCreated,
		summary.TiersUpdated,
		summary.TiersSkipped,
	)
}

func accounts() []account {
	return []account{
		{
			Name:     value("SEED_ADMIN_NAME", "Administrador"),
			Email:    value("SEED_ADMIN_EMAIL", "admin@vozkot.local"),
			Password: value("SEED_ADMIN_PASSWORD", "Admin@1234"),
			Role:     user.RoleAdmin,
		},
		{
			Name:     value("SEED_USER_NAME", "Usuario Padrao"),
			Email:    value("SEED_USER_EMAIL", "user@vozkot.local"),
			Password: value("SEED_USER_PASSWORD", "User@1234"),
			Role:     user.RoleUser,
		},
	}
}

func seed(ctx context.Context, users user.Repository, passwords auth.PasswordService, item account) (bool, error) {
	email := strings.ToLower(strings.TrimSpace(item.Email))
	existing, err := users.FindByEmail(ctx, email)
	if err != nil && !errors.Is(err, user.ErrNotFound) {
		return false, err
	}
	if existing != nil {
		return false, nil
	}
	hash, err := passwords.Hash(item.Password)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	record := &user.User{
		ID:           newID("usr"),
		Name:         strings.TrimSpace(item.Name),
		Email:        email,
		PasswordHash: hash,
		Role:         item.Role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := users.Create(ctx, record); err != nil {
		return false, err
	}
	return true, nil
}

func loadEnv() {
	if os.Getenv("APP_ENV") == "production" {
		return
	}
	if err := godotenv.Load(); err != nil {
		log.Println("godotenv: no .env file found, continuing with the environment")
	}
}

func value(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func newID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return prefix + "_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}
