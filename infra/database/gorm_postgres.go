package database

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"time"

	"vozkot/infra/config"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newLogger reports slow queries and real failures, and nothing else.
//
// "Record not found" is left out on purpose: it is the ordinary answer to a
// lookup — an order that does not exist, a key never claimed — and logging it
// as an error, with a stack line, would bury the failures that matter under
// the one that never is.
func newLogger() logger.Interface {
	return logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
	})
}

func dsn(cfg config.DatabaseConfig) string {
	endpoint := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, cfg.Port),
		Path:   cfg.Name,
	}
	query := endpoint.Query()
	query.Set("sslmode", cfg.SSLMode)
	query.Set("TimeZone", "UTC")
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

// NewApplicationDatabase creates the pool used by normal requests after
// migrations have completed.
//
// GORM's PrepareStmt is OFF, deliberately. It caches prepared statements
// across connections and, when a query text last prepared inside a
// transaction is run outside one, re-prepares it on a connection borrowed
// from the pool. A transaction that reaches that query meanwhile waits for
// the prepare while holding its own connection — and with the pool full of
// such transactions, the prepare never gets one. The load harness hit exactly
// this: every connection idle on BEGIN, nothing blocked in PostgreSQL, zero
// orders. pgx keeps its own statement cache per connection, which gives the
// same performance with no connection ever depending on another.
func NewApplicationDatabase(ctx context.Context, cfg config.DatabaseConfig) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn(cfg)), &gorm.Config{
		PrepareStmt:    false,
		TranslateError: true,
		Logger:         newLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open application database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access application connection pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping application database: %w", err)
	}
	return db, nil
}

// NewMigrationDatabase deliberately disables prepared statements and uses one
// connection. PostgreSQL DDL can invalidate cached result plans while GORM is
// introspecting and changing the same tables during AutoMigrate.
func NewMigrationDatabase(ctx context.Context, cfg config.DatabaseConfig) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn(cfg),
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		PrepareStmt:    false,
		TranslateError: true,
		Logger:         newLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open migration database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access migration connection: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping migration database: %w", err)
	}
	return db, nil
}
