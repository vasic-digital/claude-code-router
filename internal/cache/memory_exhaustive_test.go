package cache

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// --- HitCount increments monotonically per served copy ----------------------
func TestMemoryLRU_HitCountIncrements(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{})
	_ = m.Store("k", sampleEntry("k", "body"))

	for want := 1; want <= 5; want++ {
		got, ok := m.Lookup("k")
		if !ok {
			t.Fatalf("lookup %d: unexpected miss", want)
		}
		if got.HitCount != want {
			t.Fatalf("HitCount=%d want %d", got.HitCount, want)
		}
	}
}

// --- Returned entry is a copy: mutating it cannot corrupt the store ---------
func TestMemoryLRU_LookupReturnsIsolatedCopy(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{})
	_ = m.Store("k", sampleEntry("k", "body"))

	first, _ := m.Lookup("k")
	first.Model = "TAMPERED"
	first.HitCount = 9999

	second, _ := m.Lookup("k")
	if second.Model != "gpt-4o" {
		t.Fatalf("store state leaked: Model=%q", second.Model)
	}
	// HitCount reflects the store's own bookkeeping (2 lookups), not the tamper.
	if second.HitCount != 2 {
		t.Fatalf("HitCount=%d want 2 (tamper on a returned clone must not persist)", second.HitCount)
	}
}

// --- TTL expiry with an injectable clock; expiry counted exactly ------------
func TestMemoryLRU_TTLExpiryAndCounters(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	m := NewMemoryLRU(MemoryOptions{TTL: 30 * time.Second})
	m.now = clk.now

	_ = m.Store("k", sampleEntry("k", "body"))

	clk.add(29 * time.Second)
	if _, ok := m.Lookup("k"); !ok {
		t.Fatal("live before TTL")
	}
	// Exactly at the TTL boundary the entry is expired (Lookup uses !now.Before).
	clk.add(1 * time.Second)
	if _, ok := m.Lookup("k"); ok {
		t.Fatal("entry must expire at the exact TTL boundary")
	}
	s := m.Stats()
	if s.Expirations != 1 {
		t.Fatalf("Expirations=%d want 1", s.Expirations)
	}
	if s.Entries != 0 {
		t.Fatalf("Entries=%d want 0 after expiry", s.Entries)
	}
	// Lookups=2 (one hit, one expired-miss); Hits=1; Misses=1.
	if s.Lookups != 2 || s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("counters=%+v want lookups=2 hits=1 misses=1", s)
	}
}

// --- LRU eviction order under MaxEntries: strict least-recently-used --------
func TestMemoryLRU_EvictionOrderStrictLRU(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{MaxEntries: 3})
	for _, k := range []string{"a", "b", "c"} {
		_ = m.Store(k, sampleEntry(k, k))
	}
	// Access order makes "a" MRU, leaving "b" as the oldest untouched.
	_, _ = m.Lookup("a")
	_, _ = m.Lookup("c")

	// Insert "d" -> capacity exceeded -> evict LRU which is "b".
	_ = m.Store("d", sampleEntry("d", "d"))
	if _, ok := m.Lookup("b"); ok {
		t.Fatal("b (LRU) must be evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := m.Lookup(k); !ok {
			t.Fatalf("%s must survive", k)
		}
	}
	if m.Stats().Evictions != 1 {
		t.Fatalf("Evictions=%d want 1", m.Stats().Evictions)
	}
}

// --- Byte-budget eviction drops as many oldest as needed --------------------
func TestMemoryLRU_ByteBudgetMultiEviction(t *testing.T) {
	// Each body is 5 bytes; budget 12 holds at most two.
	m := NewMemoryLRU(MemoryOptions{MaxBytes: 12})
	order := []string{"a", "b", "c", "d", "e"}
	for _, k := range order {
		_ = m.Store(k, sampleEntry(k, "12345"))
	}
	// Only the last two inserted (d, e) survive; a, b, c evicted oldest-first.
	for _, gone := range []string{"a", "b", "c"} {
		if _, ok := m.Lookup(gone); ok {
			t.Fatalf("%s should have been byte-evicted", gone)
		}
	}
	for _, live := range []string{"d", "e"} {
		if _, ok := m.Lookup(live); !ok {
			t.Fatalf("%s should remain within byte budget", live)
		}
	}
	if s := m.Stats(); s.Evictions != 3 {
		t.Fatalf("Evictions=%d want 3", s.Evictions)
	}
}

// --- An entry larger than the whole byte budget is evicted immediately ------
func TestMemoryLRU_OversizedEntryEvictedImmediately(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{MaxBytes: 4})
	_ = m.Store("big", sampleEntry("big", "0123456789")) // 10 bytes > 4
	if _, ok := m.Lookup("big"); ok {
		t.Fatal("an entry exceeding the byte budget must not be retained")
	}
	if s := m.Stats(); s.Entries != 0 || s.Evictions != 1 {
		t.Fatalf("stats=%+v want entries=0 evictions=1", s)
	}
}

// --- Unbounded store never evicts ------------------------------------------
func TestMemoryLRU_UnboundedNoEviction(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{}) // MaxEntries=0, MaxBytes=0 => unbounded
	for i := 0; i < 500; i++ {
		k := strconv.Itoa(i)
		_ = m.Store(k, sampleEntry(k, k))
	}
	s := m.Stats()
	if s.Entries != 500 {
		t.Fatalf("Entries=%d want 500", s.Entries)
	}
	if s.Evictions != 0 {
		t.Fatalf("Evictions=%d want 0 for an unbounded store", s.Evictions)
	}
}

// --- Store(nil) is a benign no-op ------------------------------------------
func TestMemoryLRU_StoreNilNoop(t *testing.T) {
	m := NewMemoryLRU(MemoryOptions{})
	if err := m.Store("k", nil); err != nil {
		t.Fatalf("Store(nil) err=%v want nil", err)
	}
	if _, ok := m.Lookup("k"); ok {
		t.Fatal("Store(nil) must not create an entry")
	}
	if m.Stats().Entries != 0 {
		t.Fatal("Store(nil) must not change entry count")
	}
}

// --- Stats counter exactness across a fully-scripted sequence ---------------
func TestMemoryLRU_StatsExactSequence(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	m := NewMemoryLRU(MemoryOptions{MaxEntries: 2, TTL: 10 * time.Second})
	m.now = clk.now

	_ = m.Store("a", sampleEntry("a", "A")) // entries: a
	_ = m.Store("b", sampleEntry("b", "B")) // entries: a,b
	_, _ = m.Lookup("a")                    // hit (a MRU)
	_, _ = m.Lookup("missing")              // miss
	_ = m.Store("c", sampleEntry("c", "C")) // evict LRU "b"; entries: a,c
	_, _ = m.Lookup("b")                    // miss (evicted)

	clk.add(11 * time.Second) // everything now past TTL
	_, _ = m.Lookup("a")      // expired-miss (a dropped)
	_, _ = m.Lookup("c")      // expired-miss (c dropped)

	s := m.Stats()
	want := Stats{
		Entries:     0,
		Lookups:     5, // a, missing, b, a, c
		Hits:        1, // a (first)
		Misses:      4, // missing, b, a-expired, c-expired
		Evictions:   1, // b evicted by capacity
		Expirations: 2, // a, c expired
	}
	if s != want {
		t.Fatalf("stats=%+v want %+v", s, want)
	}
	// Invariant: Hits + Misses == Lookups.
	if s.Hits+s.Misses != s.Lookups {
		t.Fatalf("Hits+Misses=%d != Lookups=%d", s.Hits+s.Misses, s.Lookups)
	}
}

// --- Concurrency with EXACT totals on a disjoint key space ------------------
//
// Each worker owns a private key range and stores-then-looks-up each key once.
// With an unbounded store and no TTL every lookup is a guaranteed hit, so the
// final counters are fully determined despite concurrent execution.
func TestMemoryLRU_ConcurrentExactTotals(t *testing.T) {
	const workers = 8
	const perWorker = 100
	m := NewMemoryLRU(MemoryOptions{}) // unbounded, no TTL

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				k := fmt.Sprintf("w%d-k%d", w, i)
				_ = m.Store(k, sampleEntry(k, "body"))
				if _, ok := m.Lookup(k); !ok {
					t.Errorf("worker %d: own key %s missed", w, k)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	s := m.Stats()
	total := int64(workers * perWorker)
	if s.Lookups != total {
		t.Fatalf("Lookups=%d want %d", s.Lookups, total)
	}
	if s.Hits != total {
		t.Fatalf("Hits=%d want %d (every own-key lookup must hit)", s.Hits, total)
	}
	if s.Misses != 0 {
		t.Fatalf("Misses=%d want 0", s.Misses)
	}
	if s.Entries != int(total) {
		t.Fatalf("Entries=%d want %d", s.Entries, total)
	}
	if s.Hits+s.Misses != s.Lookups {
		t.Fatalf("Hits+Misses=%d != Lookups=%d", s.Hits+s.Misses, s.Lookups)
	}
}
