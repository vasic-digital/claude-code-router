package cache

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// clock is a controllable time source for deterministic TTL tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func sampleEntry(key, body string) *Entry {
	return &Entry{
		Key:          key,
		ProviderName: "openai",
		Model:        "gpt-4o",
		SystemHash:   "abc123",
		OpenAIBody:   []byte(body),
		InputTokens:  10,
		OutputTokens: 20,
	}
}

// storeFactory builds a fresh Cache and hands back the clock driving its TTL.
type storeFactory struct {
	name string
	make func(t *testing.T, ttl time.Duration, clk *clock) Cache
}

func factories() []storeFactory {
	return []storeFactory{
		{
			name: "memory",
			make: func(t *testing.T, ttl time.Duration, clk *clock) Cache {
				m := NewMemoryLRU(MemoryOptions{MaxEntries: 100, TTL: ttl})
				m.now = clk.now
				return m
			},
		},
		{
			name: "sqlite",
			make: func(t *testing.T, ttl time.Duration, clk *clock) Cache {
				path := filepath.Join(t.TempDir(), "cache.db")
				s, err := NewSQLiteCache(path, ttl)
				if err != nil {
					t.Fatalf("NewSQLiteCache: %v", err)
				}
				s.now = clk.now
				t.Cleanup(func() { _ = s.Close() })
				return s
			},
		},
	}
}

// exact hit returns the stored body; a different key misses.
func TestStore_ExactHitAndMiss(t *testing.T) {
	for _, f := range factories() {
		t.Run(f.name, func(t *testing.T) {
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			c := f.make(t, 0, clk)

			if _, ok := c.Lookup("nope"); ok {
				t.Fatal("empty store must miss")
			}

			if err := c.Store("k1", sampleEntry("k1", "hello-body")); err != nil {
				t.Fatalf("Store: %v", err)
			}

			got, ok := c.Lookup("k1")
			if !ok {
				t.Fatal("expected exact hit")
			}
			if string(got.OpenAIBody) != "hello-body" {
				t.Fatalf("body=%q want hello-body", got.OpenAIBody)
			}
			if got.Model != "gpt-4o" || got.InputTokens != 10 || got.OutputTokens != 20 {
				t.Fatalf("metadata not round-tripped: %+v", got)
			}

			if _, ok := c.Lookup("k2"); ok {
				t.Fatal("unstored key must miss (no collision)")
			}

			s := c.Stats()
			if s.Hits != 1 || s.Misses != 2 || s.Lookups != 3 {
				t.Fatalf("stats=%+v want hits=1 misses=2 lookups=3", s)
			}
		})
	}
}

// TTL expiry: a hit before expiry, a miss after.
func TestStore_TTLExpiry(t *testing.T) {
	for _, f := range factories() {
		t.Run(f.name, func(t *testing.T) {
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			c := f.make(t, 60*time.Second, clk)

			if err := c.Store("k", sampleEntry("k", "body")); err != nil {
				t.Fatalf("Store: %v", err)
			}

			// 59s later: still live.
			clk.add(59 * time.Second)
			if _, ok := c.Lookup("k"); !ok {
				t.Fatal("entry must be live before TTL")
			}

			// past 60s TTL: expired -> miss.
			clk.add(2 * time.Second)
			if _, ok := c.Lookup("k"); ok {
				t.Fatal("entry must be a miss after TTL expiry")
			}

			s := c.Stats()
			if s.Expirations != 1 {
				t.Fatalf("expected 1 expiration, got %+v", s)
			}
			if s.Entries != 0 {
				t.Fatalf("expired entry must not count as live: entries=%d", s.Entries)
			}
		})
	}
}

// an explicit per-entry ExpiresAt is honoured over the store default TTL.
func TestStore_ExplicitExpiresAt(t *testing.T) {
	for _, f := range factories() {
		t.Run(f.name, func(t *testing.T) {
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			c := f.make(t, time.Hour, clk) // long default TTL...

			e := sampleEntry("k", "body")
			e.ExpiresAt = clk.now().Add(5 * time.Second) // ...but short explicit expiry
			if err := c.Store("k", e); err != nil {
				t.Fatalf("Store: %v", err)
			}

			clk.add(6 * time.Second)
			if _, ok := c.Lookup("k"); ok {
				t.Fatal("explicit ExpiresAt must be honoured over default TTL")
			}
		})
	}
}

// Store overwrites an existing key in place.
func TestStore_Overwrite(t *testing.T) {
	for _, f := range factories() {
		t.Run(f.name, func(t *testing.T) {
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			c := f.make(t, 0, clk)

			_ = c.Store("k", sampleEntry("k", "v1"))
			_ = c.Store("k", sampleEntry("k", "v2"))

			got, ok := c.Lookup("k")
			if !ok || string(got.OpenAIBody) != "v2" {
				t.Fatalf("overwrite failed: ok=%v body=%q", ok, got.OpenAIBody)
			}
			if c.Stats().Entries != 1 {
				t.Fatalf("overwrite must not add a second entry: %+v", c.Stats())
			}
		})
	}
}

// MemoryLRU-specific: eviction under the entry bound, LRU order.
func TestMemoryLRU_Eviction(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	m := NewMemoryLRU(MemoryOptions{MaxEntries: 2})
	m.now = clk.now

	_ = m.Store("a", sampleEntry("a", "A"))
	_ = m.Store("b", sampleEntry("b", "B"))
	// touch "a" so "b" becomes least-recently-used
	if _, ok := m.Lookup("a"); !ok {
		t.Fatal("a should be present")
	}
	_ = m.Store("c", sampleEntry("c", "C")) // evicts LRU = "b"

	if _, ok := m.Lookup("b"); ok {
		t.Fatal("b should have been evicted as LRU")
	}
	if _, ok := m.Lookup("a"); !ok {
		t.Fatal("a should survive (recently used)")
	}
	if _, ok := m.Lookup("c"); !ok {
		t.Fatal("c should be present")
	}
	if m.Stats().Evictions != 1 {
		t.Fatalf("expected 1 eviction, got %+v", m.Stats())
	}
}

// MemoryLRU-specific: byte budget eviction.
func TestMemoryLRU_ByteBudget(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{MaxBytes: 8})
	_ = m.Store("a", sampleEntry("a", "12345")) // 5 bytes
	_ = m.Store("b", sampleEntry("b", "67890")) // +5 = 10 > 8 -> evict "a"

	if _, ok := m.Lookup("a"); ok {
		t.Fatal("a should be evicted by byte budget")
	}
	if _, ok := m.Lookup("b"); !ok {
		t.Fatal("b should remain")
	}
}

// SQLite persistence: entries survive reopening the same file.
func TestSQLite_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")

	s1, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.Store("k", sampleEntry("k", "durable")); err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = s1.Close()

	s2, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()

	got, ok := s2.Lookup("k")
	if !ok || string(got.OpenAIBody) != "durable" {
		t.Fatalf("entry did not persist across reopen: ok=%v body=%q", ok, got.OpenAIBody)
	}
}

// Concurrent Store/Lookup/Stats must be race-free (validated under -race).
func TestStore_ConcurrentAccess(t *testing.T) {
	for _, f := range factories() {
		t.Run(f.name, func(t *testing.T) {
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			c := f.make(t, time.Hour, clk)

			const workers = 8
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < 50; i++ {
						key := "k" + strconv.Itoa((w*50+i)%20)
						_ = c.Store(key, sampleEntry(key, "body"))
						_, _ = c.Lookup(key)
						_ = c.Stats()
					}
				}(w)
			}
			wg.Wait()
		})
	}
}

// The concrete stores satisfy the Cache interface (compile-time guarantee).
var (
	_ Cache = (*MemoryLRU)(nil)
	_ Cache = (*SQLiteCache)(nil)
)
