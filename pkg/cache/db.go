package cache

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS caches (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    key        TEXT    NOT NULL,
    version    TEXT    NOT NULL,
    size       INTEGER NOT NULL DEFAULT 0,
    complete   INTEGER NOT NULL DEFAULT 0,
    used_at    INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_caches_key_version ON caches(key, version);
CREATE INDEX IF NOT EXISTS idx_caches_complete ON caches(complete);
CREATE INDEX IF NOT EXISTS idx_caches_used_at ON caches(used_at);
`

// Cache represents a cache entry in the database.
type Cache struct {
	ID        int64
	Key       string
	Version   string
	Size      int64
	Complete  bool
	UsedAt    int64
	CreatedAt int64
}

// OpenDB opens (or creates) a SQLite database in WAL mode.
func OpenDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening cache db: %w", err)
	}
	// SQLite WAL: one writer, concurrent readers. Keep a single connection
	// for writes and let the pool handle read concurrency.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating cache schema: %w", err)
	}
	return db, nil
}

// FindCache looks up a cache entry by key prefix list and version.
// Returns nil if no match. Keys are tried in order; exact match takes priority.
func FindCache(db *sql.DB, keys []string, version string) (*Cache, error) {
	for _, prefix := range keys {
		// Exact match first.
		c, err := queryOne(db,
			`SELECT id, key, version, size, complete, used_at, created_at
			 FROM caches
			 WHERE key = ? AND version = ? AND complete = 1
			 ORDER BY created_at DESC LIMIT 1`, prefix, version)
		if err != nil {
			return nil, err
		}
		if c != nil {
			return c, nil
		}
		// Prefix match.
		c, err = queryOne(db,
			`SELECT id, key, version, size, complete, used_at, created_at
			 FROM caches
			 WHERE key LIKE ? AND version = ? AND complete = 1
			 ORDER BY created_at DESC LIMIT 1`, prefix+"%", version)
		if err != nil {
			return nil, err
		}
		if c != nil {
			return c, nil
		}
	}
	return nil, nil
}

// InsertCache creates a new cache entry and returns it with the assigned ID.
func InsertCache(db *sql.DB, c *Cache) error {
	now := time.Now().Unix()
	c.CreatedAt = now
	c.UsedAt = now
	res, err := db.Exec(
		`INSERT INTO caches (key, version, size, complete, used_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		c.Key, c.Version, c.Size, c.UsedAt, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert cache: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	c.ID = id
	return nil
}

// GetCache retrieves a cache entry by ID. Returns nil if not found.
func GetCache(db *sql.DB, id int64) (*Cache, error) {
	return queryOne(db,
		`SELECT id, key, version, size, complete, used_at, created_at
		 FROM caches WHERE id = ?`, id)
}

// CompleteCache marks a cache entry as complete with the final size.
func CompleteCache(db *sql.DB, id int64, size int64) error {
	_, err := db.Exec(`UPDATE caches SET complete = 1, size = ? WHERE id = ?`, size, id)
	return err
}

// TouchCache updates the used_at timestamp.
func TouchCache(db *sql.DB, id int64) {
	db.Exec(`UPDATE caches SET used_at = ? WHERE id = ?`, time.Now().Unix(), id) //nolint:errcheck
}

// DeleteCache removes a cache entry by ID.
func DeleteCache(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM caches WHERE id = ?`, id)
	return err
}

// FindExpired returns cache entries matching the given conditions for GC.
func FindExpired(db *sql.DB, query string, args ...any) ([]*Cache, error) {
	rows, err := db.Query(
		`SELECT id, key, version, size, complete, used_at, created_at FROM caches WHERE `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// FindDuplicates returns cache entries that have duplicates (same key+version),
// excluding the newest per group. Used for GC.
func FindDuplicates(db *sql.DB, keepOlderThan int64) ([]*Cache, error) {
	// Find all complete caches that have a newer sibling with the same key+version,
	// excluding recently-used ones.
	rows, err := db.Query(`
		SELECT c.id, c.key, c.version, c.size, c.complete, c.used_at, c.created_at
		FROM caches c
		INNER JOIN (
			SELECT key, version, MAX(created_at) as max_created
			FROM caches
			WHERE complete = 1
			GROUP BY key, version
			HAVING COUNT(*) > 1
		) g ON c.key = g.key AND c.version = g.version AND c.created_at < g.max_created
		WHERE c.complete = 1 AND c.used_at < ?`, keepOlderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

func queryOne(db *sql.DB, query string, args ...any) (*Cache, error) {
	c := &Cache{}
	err := db.QueryRow(query, args...).Scan(
		&c.ID, &c.Key, &c.Version, &c.Size, &c.Complete, &c.UsedAt, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query cache: %w", err)
	}
	return c, nil
}

func scanAll(rows *sql.Rows) ([]*Cache, error) {
	var caches []*Cache
	for rows.Next() {
		c := &Cache{}
		if err := rows.Scan(&c.ID, &c.Key, &c.Version, &c.Size, &c.Complete, &c.UsedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		caches = append(caches, c)
	}
	return caches, rows.Err()
}
