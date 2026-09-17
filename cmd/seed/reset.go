package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"gorm.io/gorm"
)

// resetCatalogue empties the catalogue so a seed starts from nothing.
//
// The seeder is idempotent, which is right for topping up a database and wrong
// for starting over: it reports what already exists and leaves it, so a second
// run against a seeded database changes nothing. Starting over needs this.
//
// WHAT IT KEEPS: users, and the sessions that let them stay signed in. Erasing
// those would be a surprise of a different order, because the person running
// this is signed in as one of them and would find themselves locked out of the
// product they were testing. The instruction is about clearing the catalogue,
// and an account is not part of it.
//
// WHAT IT ERASES, in dependency order: the money and the door first, then the
// inventory, then the rooms, then the events themselves. Ordered rather than
// TRUNCATE ... CASCADE so that a foreign key which is wrong about its own
// direction fails loudly here instead of silently taking a table with it.
func resetCatalogue(ctx context.Context, db *gorm.DB) error {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production") {
		return fmt.Errorf("refusing to reset the catalogue with APP_ENV=production")
	}

	// Each entry is a table and why it goes where it does. The comments are the
	// dependency argument; the order is the argument applied.
	steps := []struct {
		table string
		why   string
	}{
		// The door and the money. An admission names an order item, a refund
		// names an order, so both go before orders do.
		{"admissions", "a ticket at the door, which names an order item"},
		{"refund_requests", "a refund, which names an order"},
		{"order_items", "the lines of an order, which name a tier and a seat"},
		{"orders", "the purchase itself"},
		{"idempotency_keys", "replay protection for orders that no longer exist"},
		// The inventory of one night.
		{"event_seats", "the chairs an event was selling"},
		{"event_seatings", "the manifest binding a plan to a night"},
		{"tickets", "the tiers, which name an event"},
		// The rooms. Seats name sections, sections name layouts, layouts name
		// venues, so they unwind in that order.
		{"layout_seats", "the chairs of a drawn room"},
		{"layout_sections", "the blocks of a drawn room"},
		{"venue_layouts", "the drawn rooms"},
		{"venues", "the buildings"},
		// And the events, plus everything hanging off them.
		{"media", "covers and gallery images"},
		{"events", "the events themselves"},
		// Queue work about records that are gone. Left behind it would have
		// workers retrying jobs for events that no longer exist, which is a
		// log full of failures nobody caused.
		{"jobs", "queued work about records that no longer exist"},
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, step := range steps {
			if !tx.Migrator().HasTable(step.table) {
				continue
			}
			// DELETE rather than TRUNCATE: it runs inside this transaction, so a
			// failure halfway leaves the catalogue as it was rather than
			// half-erased.
			if err := tx.Exec("DELETE FROM " + step.table).Error; err != nil {
				return fmt.Errorf("clear %s (%s): %w", step.table, step.why, err)
			}
			log.Printf("cleared %s", step.table)
		}
		return nil
	})
}
