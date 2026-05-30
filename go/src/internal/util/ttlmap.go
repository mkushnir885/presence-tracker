package util

import (
	"sync"
	"time"
)

// TTLMap is a concurrency-safe map whose entries expire after a TTL, calling
// onExpire when they do (used to auto-delete abandoned Telegram prompts).
type TTLMap[K comparable, V any] struct {
	mu       sync.Mutex
	entries  map[K]*ttlEntry[V]
	onExpire func(K, V)
}

type ttlEntry[V any] struct {
	value V
	timer *time.Timer
}

func NewTTLMap[K comparable, V any](onExpire func(K, V)) *TTLMap[K, V] {
	return &TTLMap[K, V]{
		entries:  make(map[K]*ttlEntry[V]),
		onExpire: onExpire,
	}
}

func (m *TTLMap[K, V]) Put(key K, value V, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.entries[key]; ok {
		old.timer.Stop()
	}
	e := &ttlEntry[V]{value: value}
	e.timer = time.AfterFunc(ttl, func() { m.expire(key, e) })
	m.entries[key] = e
}

func (m *TTLMap[K, V]) Get(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		return e.value, true
	}
	return *new(V), false
}

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

func (m *TTLMap[K, V]) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		e.timer.Stop()
	}
	clear(m.entries)
}

// expire removes key only if it still holds self, so a stale timer can't
// drop an entry that Put already replaced under the same key.
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
