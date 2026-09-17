// Command seed creates the baseline accounts and the development catalogue a
// fresh installation needs. It is idempotent, so existing records are reported
// and left untouched instead of being overwritten.
//
// Run it with -reset to start over: the catalogue is emptied first, accounts and
// their sessions kept. That flag is destructive and says so, which is why it is
// a flag rather than the default and why it refuses to run with
// APP_ENV=production.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"vozkot/domain/auth"
	"vozkot/domain/user"
	"vozkot/infra/config"
	"vozkot/infra/crypto/pii"
	"vozkot/infra/crypto/piigorm"
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
	reset := flag.Bool("reset", false,
		"empty the catalogue before seeding, keeping accounts and sessions")
	seats := flag.Bool("seats", true,
		"also seed venues, drawn rooms and the events that sell numbered seats")
	areaEvent := flag.String("split-area-event", "", "repair the development concert layout for this event slug without resetting other data")
	flag.Parse()

	loadEnv()
	installEncryption()

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

	if *areaEvent != "" {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("area seed repair is development-only")
		}
		if err := splitSeededConcertAreas(ctx, db, *areaEvent); err != nil {
			log.Fatal(err)
		}
		log.Print("counted areas configured; no other events changed")
		return
	}

	if *reset {
		if err := resetCatalogue(ctx, db); err != nil {
			log.Fatalf("reset the catalogue: %v", err)
		}
		log.Print("catalogue emptied, accounts kept")
	}

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

	if !*seats {
		return
	}
	rooms, err := seedSeating(ctx, db, owner.ID)
	if err != nil {
		log.Fatalf("seed seating: %v", err)
	}
	log.Printf(
		"seating ready: %d venues, %d drawn rooms, %d nights selling numbered seats, %d chairs",
		rooms.Venues, rooms.Layouts, rooms.BoundEvents, rooms.Seats,
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

// installEncryption installs the keyring that seals documents, legal names and
// dates of birth, exactly as the API container does at startup.
//
// The seeder needs it to READ, not to write: no seeded account carries a
// document, but looking one up by email scans the sealed column, and a sealed
// column with no keyring is a scan error rather than an empty value. Without
// this the command died on its first account with the keys sitting unused in
// the same .env it had just loaded.
//
// Missing keys are a warning and not a stop, matching the container: a clone
// with no keys is a valid development database, and the seeder is then only
// unable to read accounts that hold sealed data. Production never reaches here,
// because -reset refuses it and a seed is not how production gets its data.
func installEncryption() {
	service, err := pii.LoadFromEnv()
	if err != nil {
		log.Printf("pii: encryption is not configured (%v); accounts holding sealed documents cannot be read", err)
		return
	}
	piigorm.SetService(service)
	log.Printf("pii: encryption active, key version %d", service.ActiveKEKVersion())
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
