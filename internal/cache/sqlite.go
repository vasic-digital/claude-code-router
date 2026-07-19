package cache

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO), registers "sqlite"
)

// sqliteDDL is the Phase 1.4 subset of the dossier schema: the exact-tier
// cache_entries table. No API key, Authorization header, or raw provider
// credential is ever written here — only provider_name + model.
//
// The semantic tier's cache_embeddings table is intentionally NOT created here:
// embeddings require the Embedder seam (semantic.go), which this package does
// not ship a production implementation for, so persisting them would be dead
// schema. It is added when the Embedder lands.
const sqliteDDL = `
CREATE TABLE IF NOT EXISTS cache_entries (
    key            TEXT    PRIMARY KEY,
    provider_name  TEXT    NOT NULL,
    model          TEXT    NOT NULL,
    system_hash    TEXT    NOT NULL,
    openai_body    BLOB    NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    hit_count      INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    generation     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cache_scope  ON cache_entries (model, system_hash);
CREATE INDEX IF NOT EXISTS idx_cache_expiry ON cache_entries (expires_at);
`

// SQLiteCache is a persistent (survives restart) Cache backed by pure-Go
// SQLite. It implements the same interface as MemoryLRU and honours TTL/expiry
// (expires_at == 0 means "no expiry"). It is safe for concurrent use; a single
// *sql.DB is used and an internal mutex serialises the counter bookkeeping.
type SQLiteCache struct {
	db  *sql.DB
	ttl time.Duration

	now func() time.Time

	mu    sync.Mutex
	stats Stats
}

// NewSQLiteCache opens (creating if needed) a SQLite cache at path. Use
// ":memory:" for an ephemeral store or a file path for persistence. TTL is
// applied to a stored Entry whose ExpiresAt is zero (0 = no expiry).
func NewSQLiteCache(path string, ttl time.Duration) (*SQLiteCache, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite cache: %w", err)
	}
	// A file DB serialises writes; keep a single connection to avoid
	// "database is locked" under the writer, which is correct and simple for a
	// single-gateway cache.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(sqliteDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init sqlite cache schema: %w", err)
	}
	return &SQLiteCache{db: db, ttl: ttl, now: time.Now}, nil
}

// Lookup implements Cache. An expired row is treated as a miss and deleted.
func (s *SQLiteCache) Lookup(key string) (*Entry, bool) {
	s.mu.Lock()
	s.stats.Lookups++
	s.mu.Unlock()

	nowUnix := s.now().Unix()
	row := s.db.QueryRow(
		`SELECT provider_name, model, system_hash, openai_body,
		        input_tokens, output_tokens, hit_count, created_at, expires_at
		   FROM cache_entries WHERE key = ?`, key)

	var (
		e         Entry
		createdAt int64
		expiresAt int64
	)
	err := row.Scan(&e.ProviderName, &e.Model, &e.SystemHash, &e.OpenAIBody,
		&e.InputTokens, &e.OutputTokens, &e.HitCount, &createdAt, &expiresAt)
	if err == sql.ErrNoRows {
		s.mu.Lock()
		s.stats.Misses++
		s.mu.Unlock()
		return nil, false
	}
	if err != nil {
		s.mu.Lock()
		s.stats.Misses++
		s.mu.Unlock()
		return nil, false
	}

	e.Key = key
	e.CreatedAt = time.Unix(createdAt, 0)
	if expiresAt != 0 {
		e.ExpiresAt = time.Unix(expiresAt, 0)
	}

	if expiresAt != 0 && nowUnix >= expiresAt {
		_, _ = s.db.Exec(`DELETE FROM cache_entries WHERE key = ?`, key)
		s.mu.Lock()
		s.stats.Expirations++
		s.stats.Misses++
		s.mu.Unlock()
		return nil, false
	}

	e.HitCount++
	_, _ = s.db.Exec(`UPDATE cache_entries SET hit_count = hit_count + 1 WHERE key = ?`, key)
	s.mu.Lock()
	s.stats.Hits++
	s.mu.Unlock()
	return &e, true
}

// Store implements Cache (INSERT OR REPLACE).
func (s *SQLiteCache) Store(key string, e *Entry) error {
	if e == nil {
		return nil
	}
	now := s.now()
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	var expiresUnix int64
	switch {
	case !e.ExpiresAt.IsZero():
		expiresUnix = e.ExpiresAt.Unix()
	case s.ttl > 0:
		expiresUnix = now.Add(s.ttl).Unix()
	default:
		expiresUnix = 0
	}

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO cache_entries
		   (key, provider_name, model, system_hash, openai_body,
		    input_tokens, output_tokens, hit_count, created_at, expires_at, generation)
		 VALUES (?,?,?,?,?,?,?,?,?,?,0)`,
		key, e.ProviderName, e.Model, e.SystemHash, e.OpenAIBody,
		e.InputTokens, e.OutputTokens, e.HitCount, createdAt.Unix(), expiresUnix)
	if err != nil {
		return fmt.Errorf("store cache entry: %w", err)
	}
	return nil
}

// Stats implements Cache. Entries counts live (non-expired) rows.
func (s *SQLiteCache) Stats() Stats {
	s.mu.Lock()
	out := s.stats
	s.mu.Unlock()

	nowUnix := s.now().Unix()
	var n int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM cache_entries WHERE expires_at = 0 OR expires_at > ?`,
		nowUnix).Scan(&n)
	out.Entries = n
	return out
}

// Close implements Cache; it closes the underlying database.
func (s *SQLiteCache) Close() error { return s.db.Close() }
