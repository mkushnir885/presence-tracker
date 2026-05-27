package util

import (
	"sync"
	"time"
)

// TTLMap is a concurrent map whose entries are removed automatically
// when their TTL elapses. The zero TTLMap is not usable — construct
// one with NewTTLMap.
type TTLMap[K comparable, V any] struct {
	mu       sync.Mutex
	entries  map[K]*ttlEntry[V]
	onExpire func(K, V)
}

type ttlEntry[V any] struct {
	value V
	timer *time.Timer
}

// NewTTLMap returns an empty TTLMap. onExpire, if non-nil, is invoked
// from a background goroutine after an entry's TTL elapses; it must
// not call back into the same TTLMap's locking methods synchronously
// or it will deadlock. Use Delete and Stop to remove entries without
// firing onExpire.
func NewTTLMap[K comparable, V any](onExpire func(K, V)) *TTLMap[K, V] {
	return &TTLMap[K, V]{
		entries:  make(map[K]*ttlEntry[V]),
		onExpire: onExpire,
	}
}

// Put stores value under key with the given TTL. If an entry already
// exists for key, its pending timer is cancelled and its onExpire is
// not fired — Put always replaces.
func (m *TTLMap[K, V]) Put(key K, value V, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.entries[key]; ok {
		old.timer.Stop()
	}
	e := &ttlEntry[V]{value: value}
	// Pointer identity discriminates this entry from any later one
	// stored under the same key, so a timer that fires after a
	// replacing Put does not act on the new entry.
	e.timer = time.AfterFunc(ttl, func() { m.expire(key, e) })
	m.entries[key] = e
}

// Get returns the value stored under key, or the zero value and false
// if no entry exists.
func (m *TTLMap[K, V]) Get(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		return e.value, true
	}
	return *new(V), false
}

// Delete removes the entry for key without firing onExpire and returns
// the removed value, if any.
func (m *TTLMap[K, V]) Delete(key K) (V, bool) {
	m.mu.Lock()
	e, ok := m.entries[key]
	if ok {
		e.timer.Stop()
		delete(m.entries, key)
	}
	m.mu.Unlock()
	if ok {
		return e.value, true
	}
	return *new(V), false
}

// Stop cancels all outstanding timers and clears the map. onExpire is
// not fired for any remaining entry. The map remains usable.
func (m *TTLMap[K, V]) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		e.timer.Stop()
	}
	clear(m.entries)
}

func (m *TTLMap[K, V]) expire(key K, self *ttlEntry[V]) {
	m.mu.Lock()
	cur, ok := m.entries[key]
	if !ok || cur != self {
		m.mu.Unlock()
		return
	}
	delete(m.entries, key)
	m.mu.Unlock()
	if m.onExpire != nil {
		m.onExpire(key, self.value)
	}
}
