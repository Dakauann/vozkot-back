package database

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	eventdomain "vozkot/domain/event"

	"gorm.io/gorm"
)

// backfillEvents gives every existing ticket tier an event to belong to.
//
// Before this migration a "ticket" was both the happening and the price: an
// evening selling Pista and Camarote was two rows that agreed about the event
// only by repeating its name in a string column. This creates one event per
// distinct (owner, event name) and points the tiers at it, taking the venue,
// city and date from the tiers themselves: they already held them, identically,
// which is exactly the duplication the event table removes.
//
// It runs inside the migration transaction and it is idempotent: it only ever
// looks at tiers that have no event yet, so a boot after the first finds
// nothing to do and does nothing.
func backfillEvents(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("tickets") || !tx.Migrator().HasTable("events") {
		return nil
	}

	type group struct {
		OwnerID   string
		EventName string
		Venue     string
		City      string
		StartsAt  time.Time
	}
	var groups []group
	// MIN over the tier columns rather than an arbitrary pick: the values are
	// meant to be identical across a group, and MIN is deterministic when an
	// operator let them drift. The earliest start is also the honest answer for
	// an event whose tiers open at different times.
	err := tx.Raw(`
		SELECT owner_id,
		       event_name,
		       MIN(COALESCE(venue, ''))  AS venue,
		       MIN(COALESCE(city, ''))   AS city,
		       MIN(starts_at)            AS starts_at
		FROM tickets
		WHERE event_id IS NULL OR event_id = ''
		GROUP BY owner_id, event_name
		ORDER BY owner_id, event_name`).Scan(&groups).Error
	if err != nil {
		return fmt.Errorf("read tiers with no event: %w", err)
	}
	if len(groups) == 0 {
		return nil
	}

	log.Printf("database: backfilling %d event(s) from existing ticket tiers", len(groups))
	for _, item := range groups {
		name := strings.TrimSpace(item.EventName)
		if name == "" {
			// A tier whose event was never named still needs somewhere to live,
			// and an operator needs to be able to find it to fix it.
			name = "Evento sem nome"
		}
		venue := item.Venue
		if venue == "" {
			venue = name
		}
		city := item.City
		if city == "" {
			// City is required on an event and was optional on a tier. An
			// operator has to correct this; a placeholder they can search for
			// beats refusing to migrate their data.
			city = "A definir"
		}
		startsAt := item.StartsAt
		if startsAt.IsZero() {
			startsAt = time.Now().UTC()
		}

		eventID := "evt_" + randomHex(8)
		slug, err := uniqueSlug(tx, eventdomain.Slugify(name), eventID)
		if err != nil {
			return err
		}

		insert := tx.Exec(`
			INSERT INTO events
				(id, owner_id, slug, name, description, category, venue, address,
				 neighborhood, city, uf, postal_code, starts_at, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, '', ?, ?, '', '', ?, '', '', ?, ?, NOW(), NOW())`,
			eventID, item.OwnerID, slug, name, string(eventdomain.CategoryOutros), venue, city,
			startsAt.UTC(), string(eventdomain.StatusPublished))
		if insert.Error != nil {
			return fmt.Errorf("create event for %q: %w", name, insert.Error)
		}

		// Only the tiers this group was read from, and only while they still
		// have no event: a concurrent boot that got here first wins, and this
		// one updates nothing.
		point := tx.Exec(`
			UPDATE tickets SET event_id = ?
			WHERE owner_id = ? AND event_name = ? AND (event_id IS NULL OR event_id = '')`,
			eventID, item.OwnerID, item.EventName)
		if point.Error != nil {
			return fmt.Errorf("point tiers at event %s: %w", eventID, point.Error)
		}
	}
	return nil
}

// moveMediaToEvents repoints artwork from a tier to that tier's event.
//
// Media used to hang off a ticket, which meant an evening selling Pista and
// Camarote could carry two posters and a listing card had to pick one. The
// column is renamed rather than added-and-copied so the rows keep their
// identity, and then every value is translated through the tier it used to
// name.
func moveMediaToEvents(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("ticket_media") {
		return nil
	}
	if !tx.Migrator().HasColumn("ticket_media", "ticket_id") {
		return nil
	}
	if tx.Migrator().HasColumn("ticket_media", "event_id") {
		// A half-finished earlier attempt. Nothing safe to guess here.
		return fmt.Errorf("ticket_media has both ticket_id and event_id; resolve by hand")
	}

	// The old foreign key points at tickets and has to go before the column it
	// constrains means something else.
	if err := tx.Exec(`ALTER TABLE ticket_media DROP CONSTRAINT IF EXISTS fk_ticket_media_ticket`).Error; err != nil {
		return fmt.Errorf("drop media constraint: %w", err)
	}
	if err := tx.Exec(`ALTER TABLE ticket_media RENAME COLUMN ticket_id TO event_id`).Error; err != nil {
		return fmt.Errorf("rename ticket_media.ticket_id: %w", err)
	}
	// Every row now holds a TIER id under a column called event_id. Translate.
	translated := tx.Exec(`
		UPDATE ticket_media
		SET event_id = tickets.event_id
		FROM tickets
		WHERE ticket_media.event_id = tickets.id`)
	if translated.Error != nil {
		return fmt.Errorf("repoint media at events: %w", translated.Error)
	}
	log.Printf("database: moved %d media row(s) from tiers to their events", translated.RowsAffected)

	// Anything left names a tier that no longer exists, so it points at
	// nothing and would fail the new foreign key. Its object is orphaned in the
	// bucket either way; the row is what would block the migration.
	orphans := tx.Exec(`
		DELETE FROM ticket_media
		WHERE NOT EXISTS (SELECT 1 FROM events WHERE events.id = ticket_media.event_id)`)
	if orphans.Error != nil {
		return fmt.Errorf("remove orphaned media: %w", orphans.Error)
	}
	if orphans.RowsAffected > 0 {
		log.Printf("database: removed %d media row(s) whose tier no longer existed", orphans.RowsAffected)
	}
	return nil
}

// addEventIDColumn adds the link as NULLABLE, which is the only way it can be
// added to a table that already has rows. enforceEventLink tightens it once the
// backfill has given every tier an event.
func addEventIDColumn(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("tickets") {
		return nil
	}
	if tx.Migrator().HasColumn("tickets", "event_id") {
		return nil
	}
	if err := tx.Exec(`ALTER TABLE tickets ADD COLUMN event_id varchar(32)`).Error; err != nil {
		return fmt.Errorf("add tickets.event_id: %w", err)
	}
	return nil
}

// uniqueSlug finds an address no other event is already using.
//
// Two operators selling "Festival Aurora" is ordinary, and the second one must
// still get a working page. The id's tail is appended rather than a counter,
// because a counter has to be read from the table under a lock to be correct
// and a random tail does not.
func uniqueSlug(tx *gorm.DB, base, eventID string) (string, error) {
	candidate := base
	for attempt := 0; attempt < 3; attempt++ {
		var taken int64
		if err := tx.Raw(`SELECT COUNT(*) FROM events WHERE slug = ?`, candidate).Scan(&taken).Error; err != nil {
			return "", fmt.Errorf("check slug %q: %w", candidate, err)
		}
		if taken == 0 {
			return candidate, nil
		}
		suffix := strings.TrimPrefix(eventID, "evt_")
		if len(suffix) > 6 {
			suffix = suffix[:6]
		}
		candidate = base + "-" + suffix
		if attempt > 0 {
			candidate = base + "-" + randomHex(4)
		}
	}
	return base + "-" + randomHex(6), nil
}

// enforceEventLink applies the constraints that could only be added once every
// tier actually had an event.
//
// Adding them before the backfill would refuse to migrate a database that has
// customers in it, which is the only kind worth migrating carefully.
func enforceEventLink(tx *gorm.DB) error {
	// A database that has no tickets table yet has nothing to enforce.
	//
	// This runs BEFORE the AutoMigrate that creates the table, which is
	// correct for an existing deployment and was silently wrong for a brand
	// new one: on an empty database the COUNT below failed with "relation
	// tickets does not exist" and took the whole migration with it. It went
	// unnoticed because every database this had ever run against already had
	// the table — until the test suite was pointed at a fresh one of its own.
	// A first deploy would have hit exactly this.
	if !tx.Migrator().HasTable("tickets") {
		return nil
	}

	var orphans int64
	if err := tx.Raw(`SELECT COUNT(*) FROM tickets WHERE event_id IS NULL OR event_id = ''`).Scan(&orphans).Error; err != nil {
		return fmt.Errorf("count tiers with no event: %w", err)
	}
	if orphans > 0 {
		// Refusing is right: a tier with no event cannot be listed, cannot be
		// found, and cannot be bought. Starting anyway would hide that until a
		// buyer noticed.
		return fmt.Errorf("%d ticket tier(s) still have no event; the backfill did not complete", orphans)
	}

	var alreadyNotNull int64
	err := tx.Raw(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'tickets' AND column_name = 'event_id' AND is_nullable = 'NO'`).Scan(&alreadyNotNull).Error
	if err != nil {
		return fmt.Errorf("inspect tickets.event_id: %w", err)
	}
	if alreadyNotNull > 0 {
		return nil
	}
	if err := tx.Exec(`ALTER TABLE tickets ALTER COLUMN event_id SET NOT NULL`).Error; err != nil {
		return fmt.Errorf("require an event on every tier: %w", err)
	}
	return nil
}

// relaxSupersededTicketColumns lets the columns that moved up to the event stop
// being required on a tier.
//
// They are not dropped in the same release that stops writing them: a rollback
// to the previous binary has to find its data intact, and a column that was
// dropped is a rollback that loses every event's date. Dropping them is a later
// migration, once the previous version can no longer be deployed.
func relaxSupersededTicketColumns(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("tickets") {
		return nil
	}
	for _, column := range []string{"event_name", "venue", "city", "starts_at"} {
		if !tx.Migrator().HasColumn("tickets", column) {
			continue
		}
		var required int64
		err := tx.Raw(`
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_name = 'tickets' AND column_name = ? AND is_nullable = 'NO'`, column).Scan(&required).Error
		if err != nil {
			return fmt.Errorf("inspect tickets.%s: %w", column, err)
		}
		if required == 0 {
			continue
		}
		if err := tx.Exec(`ALTER TABLE tickets ALTER COLUMN ` + column + ` DROP NOT NULL`).Error; err != nil {
			return fmt.Errorf("relax tickets.%s: %w", column, err)
		}
	}
	return nil
}

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))
	}
	return hex.EncodeToString(buffer)
}
