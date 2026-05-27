package util

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ttlSink collects (key, value) pairs delivered to the onExpire
// callback so tests can assert on them deterministically.
type ttlSink struct {
	mu     sync.Mutex
	events []ttlSinkEvent
	wg     sync.WaitGroup
}

type ttlSinkEvent struct {
	key int
	val string
}

func newTTLSink(expectedExpiries int) *ttlSink {
	s := &ttlSink{}
	s.wg.Add(expectedExpiries)
	return s
}

func (s *ttlSink) record(k int, v string) {
	s.mu.Lock()
	s.events = append(s.events, ttlSinkEvent{k, v})
	s.mu.Unlock()
	s.wg.Done()
}

// wait blocks until all expected expirations fire or timeout elapses.
func (s *ttlSink) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for expirations after %s", timeout)
	}
}

func TestTTLMapExpireFiresCallbackAndRemovesEntry(t *testing.T) {
	sink := newTTLSink(1)
	m := NewTTLMap(sink.record)

	m.Put(1, "one", 20*time.Millisecond)
	sink.wait(t, time.Second)

	if _, ok := m.Get(1); ok {
		t.Errorf("entry still present after expiry")
	}
	if len(sink.events) != 1 || sink.events[0] != (ttlSinkEvent{1, "one"}) {
		t.Errorf("got events %+v, want [{1 one}]", sink.events)
	}
}

func TestTTLMapDeleteSuppressesCallback(t *testing.T) {
	var fired atomic.Int32
	m := NewTTLMap(func(int, string) { fired.Add(1) })

	m.Put(1, "one", 50*time.Millisecond)
	v, ok := m.Delete(1)
	if !ok || v != "one" {
		t.Fatalf("Delete = (%q, %v), want (one, true)", v, ok)
	}

	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("onExpire fired %d times after Delete", fired.Load())
	}
	if _, ok := m.Get(1); ok {
		t.Errorf("entry still present after Delete")
	}
}

func TestTTLMapPutReplacesAndCancelsPreviousTimer(t *testing.T) {
	sink := newTTLSink(1) // only the second entry should expire
	m := NewTTLMap(sink.record)

	m.Put(1, "old", 30*time.Millisecond)
	// Replace well before old's TTL — old's onExpire must not fire.
	m.Put(1, "new", 60*time.Millisecond)

	sink.wait(t, time.Second)
	if len(sink.events) != 1 || sink.events[0].val != "new" {
		t.Errorf("got events %+v, want only [{1 new}]", sink.events)
	}
}

func TestTTLMapStopClearsAndSuppressesCallbacks(t *testing.T) {
	var fired atomic.Int32
	m := NewTTLMap(func(int, string) { fired.Add(1) })

	for i := range 5 {
		m.Put(i, "v", 30*time.Millisecond)
	}
	m.Stop()

	time.Sleep(80 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("onExpire fired %d times after Stop", fired.Load())
	}
	if _, ok := m.Get(0); ok {
		t.Errorf("entry survived Stop")
	}
}

func TestTTLMapGetBeforeExpiry(t *testing.T) {
	m := NewTTLMap[int, string](nil)
	m.Put(1, "still-here", time.Hour)

	v, ok := m.Get(1)
	if !ok || v != "still-here" {
		t.Errorf("Get = (%q, %v), want (still-here, true)", v, ok)
	}
}

func TestTTLMapNilOnExpireIsSafe(t *testing.T) {
	m := NewTTLMap[int, string](nil)
	m.Put(1, "one", 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if _, ok := m.Get(1); ok {
		t.Errorf("entry not removed when onExpire is nil")
	}
}
