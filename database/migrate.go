package database

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is a parsed embedded migration file.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies any embedded migrations whose version is higher than
// the current max value in schema_migrations. Each migration runs in
// its own transaction along with the schema_migrations row insert, so
// a failing migration leaves the schema untouched.
func (db *DB) migrate() error {
	// The runner owns the schema_migrations bootstrap; migration files
	// only carry application schema. Create it up front so the version
	// lookup below is unconditional on a fresh DB.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	current, err := db.currentVersion()
	if err != nil {
		return err
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := db.applyMigration(m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func (db *DB) currentVersion() (int, error) {
	var v sql.NullInt64
	row := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`)
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("read current version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

func (db *DB) applyMigration(m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(m.sql); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, m.version); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
}

// loadMigrations reads and parses every *.sql file under migrations/,
// returning them sorted by version ascending. Filenames must start with
// a number followed by an underscore, e.g. "001_init.sql".
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := parseMigrationVersion(e.Name())
		if err != nil {
			return nil, err
		}
		data, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseMigrationVersion(filename string) (int, error) {
	underscore := strings.IndexByte(filename, '_')
	if underscore <= 0 {
		return 0, fmt.Errorf("migration %q missing version prefix", filename)
	}
	v, err := strconv.Atoi(filename[:underscore])
	if err != nil {
		return 0, fmt.Errorf("migration %q has non-numeric version: %w", filename, err)
	}
	return v, nil
}
