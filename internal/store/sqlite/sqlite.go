// Package sqlite implements store.Store on top of embedded SQLite databases.
// The schemas are managed by goose migrations embedded in migrations/ (the
// activity database) and capture_migrations/ (the captures database) and
// applied when a store is opened.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/mostlygeek/llama-swap/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed capture_migrations/*.sql
var captureMigrationFS embed.FS

// Store is a store.Store backed by SQLite databases: an activity database, a
// cache database, and a separate captures database. Activity and cache are
// usually the same file, but they can differ so activity stays in memory while
// the model capability cache remains on disk.
type Store struct {
	// db is the cache database. It remains the historical handle used by
	// tests and is the same handle as activityDB when the two share a file.
	db         *sql.DB
	activityDB *sql.DB
	capturesDB *sql.DB
	inMemory   bool
	activity   *activityRepository
	cache      *cacheRepository
	captures   *captureRepository
}

var _ store.Store = (*Store)(nil)

// New opens a SQLite store and applies pending migrations. path is used for
// both activity and cache rows; capturesPath is the captures database. An
// empty path keeps that database in memory, lost on Close.
//
// The databases are independent: activity rows name their capture by capture
// ID, and a missing capture is not an error.
func New(path, capturesPath string) (*Store, error) {
	return NewWithActivity(path, path, capturesPath)
}

// NewWithActivity opens a store with separate activity and cache databases.
// An empty activityPath keeps activity in memory while a non-empty cachePath
// persists the model capability cache. When the paths are equal, one database
// is opened and shared.
func NewWithActivity(activityPath, cachePath, capturesPath string) (*Store, error) {
	ctx := context.Background()

	var activityDB, cacheDB *sql.DB
	var activityOnDisk bool
	var err error
	if activityPath == cachePath {
		cacheDB, activityOnDisk, err = openDB(ctx, cachePath, migrationFS, "migrations")
		if err != nil {
			return nil, err
		}
		activityDB = cacheDB
	} else {
		activityDB, activityOnDisk, err = openDB(ctx, activityPath, migrationFS, "migrations")
		if err != nil {
			return nil, err
		}
		cacheDB, _, err = openDB(ctx, cachePath, migrationFS, "migrations")
		if err != nil {
			activityDB.Close()
			return nil, err
		}
	}

	capturesDB, _, err := openDB(ctx, capturesPath, captureMigrationFS, "capture_migrations")
	if err != nil {
		if activityDB == cacheDB {
			cacheDB.Close()
		} else {
			activityDB.Close()
			cacheDB.Close()
		}
		return nil, fmt.Errorf("captures: %w", err)
	}

	fail := func(err error) (*Store, error) {
		capturesDB.Close()
		if activityDB == cacheDB {
			cacheDB.Close()
		} else {
			activityDB.Close()
			cacheDB.Close()
		}
		return nil, err
	}

	captures, err := newCaptureRepository(ctx, capturesDB)
	if err != nil {
		return fail(err)
	}
	s := &Store{
		db:         cacheDB,
		activityDB: activityDB,
		capturesDB: capturesDB,
		inMemory:   !activityOnDisk,
		activity:   &activityRepository{db: activityDB},
		cache:      &cacheRepository{db: cacheDB},
		captures:   captures,
	}
	// Drop cache rows that expired while the process was not running, so a
	// long-lived database does not accumulate them.
	if err := s.cache.Prune(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

// openDB opens the SQLite database at path, or an in-memory one when path is
// empty, and applies the migrations in dir of migrations. diskFile reports
// whether the database is on disk.
func openDB(ctx context.Context, path string, migrations fs.FS, dir string) (db *sql.DB, diskFile bool, err error) {
	dsn := strings.TrimSpace(path)
	diskFile = dsn != ""
	if dsn == "" {
		dsn = ":memory:"
	}

	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, fmt.Errorf("open sqlite store: %w", err)
	}
	// A single connection keeps an in-memory database alive (each new
	// connection to ":memory:" would be a fresh empty database) and
	// serializes writers for on-disk files.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, false, fmt.Errorf("configure sqlite store: %w", err)
	}
	if diskFile {
		var mode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
			db.Close()
			return nil, false, fmt.Errorf("enable sqlite store WAL mode: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			db.Close()
			return nil, false, fmt.Errorf("enable sqlite store WAL mode: got %q", mode)
		}
	}
	if err := runMigrations(ctx, db, migrations, dir); err != nil {
		db.Close()
		return nil, false, err
	}
	return db, diskFile, nil
}

func runMigrations(ctx context.Context, db *sql.DB, migrationsFS fs.FS, dir string) error {
	migrations, err := fs.Sub(migrationsFS, dir)
	if err != nil {
		return fmt.Errorf("sqlite store migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
	if err != nil {
		return fmt.Errorf("sqlite store migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("sqlite store migrations up: %w", err)
	}
	return nil
}

// Activity implements store.Store.
func (s *Store) Activity() store.ActivityRepository {
	return s.activity
}

// Cache implements store.Store.
func (s *Store) Cache() store.CacheRepository {
	return s.cache
}

// Captures implements store.Store.
func (s *Store) Captures() store.CaptureRepository {
	return s.captures
}

// IsInMemory implements store.Store.
func (s *Store) IsInMemory() bool {
	return s.inMemory
}

// Close implements store.Store.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.activityDB == s.db {
		return errors.Join(s.db.Close(), s.capturesDB.Close())
	}
	return errors.Join(s.activityDB.Close(), s.db.Close(), s.capturesDB.Close())
}
