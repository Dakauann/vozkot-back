package database

import (
	"fmt"

	"gorm.io/gorm"
)

// relaxPasswordColumn lets an account exist without one.
//
// Sign-in is a code sent to an address; there is no password to store for an
// account created that way. The column stays, the seeded operator accounts
// still have one, and removing it would lock them out, but it stops being
// required, and its default becomes the empty string so an insert that omits it
// is legal.
//
// It runs BEFORE AutoMigrate, in the window where the new code is already
// inserting users with no password while the old NOT NULL still stands.
func relaxPasswordColumn(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("users") || !tx.Migrator().HasColumn("users", "password_hash") {
		return nil
	}

	// Read the column's CURRENT shape before touching it.
	//
	// ALTER TABLE takes an ACCESS EXCLUSIVE lock, which queues every read and
	// write to `users` behind it, including the sign-ins happening during a
	// rolling deploy. Issuing it unconditionally means every replica takes that
	// lock on every boot, forever, to make a change that was already made. So
	// the statements below run once, on the boot that actually needs them, and
	// a steady-state boot issues no DDL at all.
	var current struct {
		IsNullable    string
		ColumnDefault *string
	}
	err := tx.Raw(`
		SELECT is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'password_hash'`).Scan(&current).Error
	if err != nil {
		return fmt.Errorf("inspect password column: %w", err)
	}

	if current.IsNullable == "NO" {
		if err := tx.Exec(`ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL`).Error; err != nil {
			return fmt.Errorf("relax password column: %w", err)
		}
	}
	if current.ColumnDefault == nil {
		if err := tx.Exec(`ALTER TABLE users ALTER COLUMN password_hash SET DEFAULT ''`).Error; err != nil {
			return fmt.Errorf("default password column: %w", err)
		}
	}
	return nil
}
