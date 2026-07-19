package cache

import (
	"container/list"
	"sync"
	"time"
)

// MemoryLRU is a size- and byte-bounded, TTL-respecting, in-memory Cache. It
// has ZERO third-party dependencies (stdlib container/list + a map), so the
// exact-match tier ships before any SQLite decision is forced.
//
// It is safe for concurrent use.
type MemoryLRU struct {
	mu         sync.Mutex
	ll         *list.List               // front = most-recently-used
	items      map[string]*list.Element // key → element (element.Value is *Entry)
	maxEntries int
	maxBytes   int64
	curBytes   int64
	ttl        time.Duration

	// now is the clock; overridable in tests to exercise TTL expiry
	// deterministically. Defaults to time.Now.
	now func() time.Time

	stats Stats
}

// MemoryOptions configures a MemoryLRU. Zero values mean "unbounded" for the
// numeric bounds and "no expiry" for TTL.
type MemoryOptions struct {
	// MaxEntries caps the number of live entries; 0 = unbounded.
	MaxEntries int
	// MaxBytes caps the total of len(OpenAIBody) across entries; 0 = unbounded.
	MaxBytes int64
	// TTL is applied to a stored Entry whose ExpiresAt is zero; 0 = no expiry.
	TTL time.Duration
}

// NewMemoryLRU builds an in-memory cache with the given options.
func NewMemoryLRU(opt MemoryOptions) *MemoryLRU {
	return &MemoryLRU{
		ll:         list.New(),
		items:      make(map[string]*list.Element),
		maxEntries: opt.MaxEntries,
		maxBytes:   opt.MaxBytes,
		ttl:        opt.TTL,
		now:        time.Now,
	}
}

// Lookup implements Cache.
func (m *MemoryLRU) Lookup(key string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stats.Lookups++
	el, ok := m.items[key]
	if !ok {
		m.stats.Misses++
		return nil, false
	}
	e := el.Value.(*Entry)
	if e.expired(m.now()) {
		m.removeElement(el)
		m.stats.Expirations++
		m.stats.Misses++
		return nil, false
	}
	m.ll.MoveToFront(el)
	e.HitCount++
	m.stats.Hits++
	return e.clone(), true
}

// Store implements Cache.
func (m *MemoryLRU) Store(key string, e *Entry) error {
	if e == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	stored := e.clone()
	stored.Key = key
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	if stored.ExpiresAt.IsZero() && m.ttl > 0 {
		stored.ExpiresAt = now.Add(m.ttl)
	}

	if el, ok := m.items[key]; ok {
		prev := el.Value.(*Entry)
		m.curBytes -= int64(len(prev.OpenAIBody))
		el.Value = stored
		m.curBytes += int64(len(stored.OpenAIBody))
		m.ll.MoveToFront(el)
	} else {
		el := m.ll.PushFront(stored)
		m.items[key] = el
		m.curBytes += int64(len(stored.OpenAIBody))
	}

	m.evictLocked()
	return nil
}

// evictLocked drops least-recently-used entries until the entry- and byte-
// bounds are satisfied. Caller holds the lock.
func (m *MemoryLRU) evictLocked() {
	for m.maxEntries > 0 && m.ll.Len() > m.maxEntries {
		if !m.evictOldest() {
			break
		}
	}
	for m.maxBytes > 0 && m.curBytes > m.maxBytes && m.ll.Len() > 0 {
		if !m.evictOldest() {
			break
		}
	}
}

func (m *MemoryLRU) evictOldest() bool {
	el := m.ll.Back()
	if el == nil {
		return false
	}
	m.removeElement(el)
	m.stats.Evictions++
	return true
}

func (m *MemoryLRU) removeElement(el *list.Element) {
	e := el.Value.(*Entry)
	m.ll.Remove(el)
	delete(m.items, e.Key)
	m.curBytes -= int64(len(e.OpenAIBody))
}

// Stats implements Cache. Entries reflects the current live count.
func (m *MemoryLRU) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats
	s.Entries = m.ll.Len()
	return s
}

// Close implements Cache. It is a no-op for the in-memory store.
func (m *MemoryLRU) Close() error { return nil }
