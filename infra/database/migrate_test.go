package database_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"vozkot/infra/database"
	"vozkot/infra/testsupport"
)

// A steady-state boot must issue NO schema change at all, and this test is here
// because that rule has been broken twice and cost a deadlock both times.
//
// The mechanism is always the same. Any DDL: ALTER, CREATE INDEX, ADD
// CONSTRAINT: takes a lock on its table that every writer has to wait for. The
// migration holds it inside a transaction that already touched another table,
// so it takes two locks in an order no running request shares, and PostgreSQL
// resolves the cycle by killing somebody's checkout. That is survivable exactly
// once, on the deploy that genuinely changes the schema. Repeated on every boot
// it is a permanent hazard, and a rolling deploy makes it a certainty.
//
// The subtle way in is a struct tag that disagrees with the column it describes:
// GORM then "reconciles" them forever, one ALTER per boot, and nothing in the
// code review looks wrong.

// recordingLogger captures the statements a migration issues.
type recordingLogger struct {
	gormlogger.Interface
	mu         sync.Mutex
	statements []string
}

func (l *recordingLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sql, _ := fc()
	l.mu.Lock()
	l.statements = append(l.statements, sql)
	l.mu.Unlock()
	l.Interface.Trace(ctx, begin, fc, err)
}

// schemaChanges returns the DDL among the recorded statements.
//
// CREATE EXTENSION is excluded: it is an IF NOT EXISTS no-op against a database
// that already has the extension, takes no table lock, and is the one statement
// that has to be attempted before the catalogue can be asked about it.
func (l *recordingLogger) schemaChanges() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var ddl []string
	for _, statement := range l.statements {
		upper := strings.ToUpper(strings.TrimSpace(statement))
		switch {
		case strings.HasPrefix(upper, "CREATE EXTENSION"):
			continue
		case strings.HasPrefix(upper, "ALTER TABLE"),
			strings.HasPrefix(upper, "CREATE TABLE"),
			strings.HasPrefix(upper, "CREATE INDEX"),
			strings.HasPrefix(upper, "CREATE UNIQUE INDEX"),
			strings.HasPrefix(upper, "DROP INDEX"),
			strings.HasPrefix(upper, "DROP TABLE"):
			ddl = append(ddl, statement)
		}
	}
	return ddl
}

func TestASteadyStateBootIssuesNoSchemaChange(t *testing.T) {
	cfg := testsupport.DatabaseConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The first run brings the schema up to date, whatever state this database
	// was left in. Whether it changes anything is not what is under test.
	first, err := database.NewMigrationDatabase(ctx, cfg)
	if err != nil {
		t.Skipf("PostgreSQL is not reachable (%v). Start it with: docker compose up -d database", err)
	}
	if err := database.RunMigrations(ctx, first); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	closeDB(t, first)

	// The second run is the one that matters: the schema already matches, so
	// there is nothing legitimate left to change.
	second, err := database.NewMigrationDatabase(ctx, cfg)
	if err != nil {
		t.Fatalf("open second migration connection: %v", err)
	}
	defer closeDB(t, second)

	recorder := &recordingLogger{Interface: second.Logger}
	second.Logger = recorder

	if err := database.RunMigrations(ctx, second); err != nil {
		t.Fatalf("second migration: %v", err)
	}

	if changes := recorder.schemaChanges(); len(changes) > 0 {
		t.Fatalf("a boot against an up-to-date schema issued %d schema change(s), "+
			"each one a table lock taken against live traffic:\n  %s",
			len(changes), strings.Join(changes, "\n  "))
	}
}

func closeDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		return
	}
	_ = sqlDB.Close()
}
