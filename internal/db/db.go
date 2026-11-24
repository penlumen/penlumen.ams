// Package db opens the app's own database, SQLite (the default: one file,
// nothing to install) or Postgres, and keeps its schema up to date.
//
// Each has its own migrations, in migrations/sqlite and migrations/postgres,
// built into the binary. A schema change needs a migration in both.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers "sqlite"; pure Go, so no C compiler is needed
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrations embed.FS

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// DefaultURL is used when DATABASE_URL isn't set: a SQLite file in the
// working directory.
const DefaultURL = "sqlite:penlumen_ams.db"

type DB struct {
	*sql.DB
	Dialect Dialect
}

// Open connects and checks the database answers. url is a Postgres URL
// (postgres://…) or a SQLite file: sqlite:path/to/penlumen_ams.db, or just the path.
func Open(ctx context.Context, url string) (*DB, error) {
	dialect, driver, dsn, err := parse(url)
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &DB{DB: conn, Dialect: dialect}, nil
}

func parse(url string) (dialect Dialect, driver, dsn string, err error) {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		return Postgres, "pgx", url, nil
	}
	path := strings.TrimPrefix(strings.TrimPrefix(url, "sqlite:"), "//")
	if path == "" {
		return "", "", "", fmt.Errorf("database: DATABASE_URL %q has no file name", url)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", "", fmt.Errorf("database: %w", err)
		}
	}
	// Foreign keys on (deleting an assessment removes its attempts), WAL so
	// students saving answers don't block each other's reads, a wait instead
	// of "database is locked" when two write at once, and write transactions
	// that take the lock up front so they never fail halfway.
	return SQLite, "sqlite", path +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)&_txlock=immediate", nil
}

// Migrate applies any migrations the database doesn't have yet.
func (d *DB) Migrate() error {
	goose.SetBaseFS(migrations)
	dialect := "postgres"
	if d.Dialect == SQLite {
		dialect = "sqlite3"
	}
	if err := goose.SetDialect(dialect); err != nil {
		return err
	}
	dir := "migrations/" + string(d.Dialect)
	// A database ahead of this build's migrations was made before they were
	// squashed into one (or by a newer build): its tables don't match.
	known, err := goose.CollectMigrations(dir, 0, goose.MaxVersion)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	current, err := goose.GetDBVersion(d.DB)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if latest := known[len(known)-1].Version; current > latest {
		return fmt.Errorf("database: its schema is version %d but this build of AMS knows up to %d, so it "+
			"was made by an older development build (before the migrations were squashed) or a newer one; "+
			"point DATABASE_URL at a new, empty database", current, latest)
	}
	if err := goose.Up(d.DB, dir); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}
