package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- Full metadata survives Close/reopen on a real temp-file DB -------------
func TestSQLite_MetadataPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")

	s1, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	e := &Entry{
		Key:          "k",
		ProviderName: "openai",
		Model:        "gpt-4o",
		SystemHash:   "scope-xyz",
		OpenAIBody:   []byte(`{"choices":[{"message":{"content":"persisted"}}]}`),
		InputTokens:  111,
		OutputTokens: 222,
	}
	if err := s1.Store("k", e); err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = s1.Close()

	s2, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()

	got, ok := s2.Lookup("k")
	if !ok {
		t.Fatal("entry lost across reopen")
	}
	if got.ProviderName != "openai" || got.Model != "gpt-4o" || got.SystemHash != "scope-xyz" {
		t.Fatalf("scope metadata not persisted: %+v", got)
	}
	if got.InputTokens != 111 || got.OutputTokens != 222 {
		t.Fatalf("token usage not persisted: in=%d out=%d", got.InputTokens, got.OutputTokens)
	}
	if string(got.OpenAIBody) != string(e.OpenAIBody) {
		t.Fatalf("body not persisted byte-for-byte: %q", got.OpenAIBody)
	}
}

// --- TTL is honoured across a reopen (expiry is stored absolutely) ----------
func TestSQLite_TTLHonouredAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ttl.db")
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	s1, err := NewSQLiteCache(path, 60*time.Second)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	s1.now = clk.now
	if err := s1.Store("k", sampleEntry("k", "body")); err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = s1.Close()

	// Reopen and jump the clock PAST the absolute expiry recorded in the file.
	s2, err := NewSQLiteCache(path, 60*time.Second)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()
	s2.now = func() time.Time { return clk.t.Add(61 * time.Second) }

	if _, ok := s2.Lookup("k"); ok {
		t.Fatal("entry must be a miss: its absolute expiry survived the reopen")
	}
	if s := s2.Stats(); s.Expirations != 1 {
		t.Fatalf("Expirations=%d want 1 after reopen", s.Expirations)
	}
	// A live reopen (clock before expiry) must still hit — control case.
	s3, err := NewSQLiteCache(path, 60*time.Second)
	if err != nil {
		// The expired row was deleted above; storing fresh proves liveness path.
		t.Fatalf("open3: %v", err)
	}
	defer s3.Close()
	s3.now = clk.now
	if err := s3.Store("live", sampleEntry("live", "body")); err != nil {
		t.Fatalf("store live: %v", err)
	}
	s3.now = func() time.Time { return clk.t.Add(30 * time.Second) }
	if _, ok := s3.Lookup("live"); !ok {
		t.Fatal("entry within TTL after reopen must hit")
	}
}

// --- HitCount persists and keeps incrementing across reopen -----------------
func TestSQLite_HitCountPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hits.db")

	s1, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	_ = s1.Store("k", sampleEntry("k", "body"))
	if got, _ := s1.Lookup("k"); got.HitCount != 1 {
		t.Fatalf("HitCount=%d want 1", got.HitCount)
	}
	if got, _ := s1.Lookup("k"); got.HitCount != 2 {
		t.Fatalf("HitCount=%d want 2", got.HitCount)
	}
	_ = s1.Close()

	s2, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()
	// Third lookup continues from the persisted count of 2 -> 3.
	if got, ok := s2.Lookup("k"); !ok || got.HitCount != 3 {
		t.Fatalf("persisted HitCount continuation: ok=%v HitCount=%d want 3", ok, got.HitCount)
	}
}

// --- A bad path errors cleanly at construction (no panic) -------------------
func TestSQLite_BadPathErrorsCleanly(t *testing.T) {
	// Make the PARENT a regular file so the DB path cannot be created.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "iamafile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	badPath := filepath.Join(notADir, "cache.db") // parent is a file

	s, err := NewSQLiteCache(badPath, 0)
	if err == nil {
		if s != nil {
			_ = s.Close()
		}
		t.Fatal("expected an error opening a DB under a non-directory parent")
	}
	if s != nil {
		t.Fatal("a failed constructor must not return a usable cache")
	}
}

// --- A corrupt DB file errors cleanly at construction (no panic) ------------
func TestSQLite_CorruptFileErrorsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	// Write bytes that carry a plausible-looking but invalid SQLite header so the
	// driver rejects the file rather than treating it as empty.
	junk := append([]byte("SQLite format 3\x00"), []byte("\xde\xad\xbe\xefnot a real database page")...)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	s, err := NewSQLiteCache(path, 0)
	if err == nil {
		// Some drivers defer corruption detection; if construction "succeeds",
		// the first operation must still fail cleanly without panicking.
		defer s.Close()
		if _, ok := s.Lookup("k"); ok {
			t.Fatal("corrupt DB must not report a hit")
		}
		if storeErr := s.Store("k", sampleEntry("k", "b")); storeErr == nil {
			t.Skip("driver tolerated the crafted file; no clean-error path to assert")
		}
		return
	}
	if s != nil {
		t.Fatal("a failed constructor must not return a usable cache")
	}
}

// --- Operations after Close fail cleanly (no panic) -------------------------
func TestSQLite_OperationsAfterCloseNoPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.db")
	s, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Store("k", sampleEntry("k", "body"))
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A store on a closed DB must surface an error, not panic.
	if err := s.Store("k2", sampleEntry("k2", "body")); err == nil {
		t.Fatal("Store on a closed DB should error")
	}
	// A lookup on a closed DB must degrade to a clean miss, not panic.
	if _, ok := s.Lookup("k"); ok {
		t.Fatal("Lookup on a closed DB should miss")
	}
	// Stats on a closed DB must not panic (Entries count may be zero).
	_ = s.Stats()
}

// --- Concurrency with EXACT final entry count on a disjoint key space -------
func TestSQLite_ConcurrentExactEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.db")
	s, err := NewSQLiteCache(path, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const workers = 8
	const perWorker = 40
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				k := fmt.Sprintf("w%d-k%d", w, i)
				if err := s.Store(k, sampleEntry(k, "body")); err != nil {
					t.Errorf("store %s: %v", k, err)
					return
				}
				if _, ok := s.Lookup(k); !ok {
					t.Errorf("own key %s missed", k)
					return
				}
				_ = s.Stats()
			}
		}(w)
	}
	wg.Wait()

	if got := s.Stats().Entries; got != workers*perWorker {
		t.Fatalf("Entries=%d want %d (disjoint keys, no expiry)", got, workers*perWorker)
	}
}
