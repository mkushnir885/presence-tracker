package messengers

import (
	"context"
	"log/slog"
	"sync"
)

// EventHandler consumes events emitted by a Messenger.
type EventHandler interface {
	HandleMessengerEvent(ctx context.Context, evt Event)
}

// Router owns a Messenger's lifetime and forwards its events to a
// swappable handler. The messenger runs for the whole daemon process so
// registrations work before any meeting starts; the handler is
// (re-)installed by whichever session is currently active.
//
// Events that arrive while no handler is set are dropped silently —
// the adapter has already persisted /register through the Registry, and
// join confirmations / answers outside an active session are not actionable.
type Router struct {
	m    Messenger
	done chan struct{}

	mu sync.RWMutex
	h  EventHandler
}

// NewRouter wraps m. The Messenger is not started until Start is called.
func NewRouter(m Messenger) *Router {
	return &Router{m: m, done: make(chan struct{})}
}

// Messenger returns the underlying Messenger so the active session can
// send DMs and challenges through it.
func (r *Router) Messenger() Messenger { return r.m }

// SetHandler installs h as the current event target. Pass nil to detach.
func (r *Router) SetHandler(h EventHandler) {
	r.mu.Lock()
	r.h = h
	r.mu.Unlock()
}

func (r *Router) handler() EventHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.h
}

// Start launches the Messenger and begins forwarding events on a
// background goroutine that exits when ctx is cancelled or the
// Messenger closes its event channel.
func (r *Router) Start(ctx context.Context) error {
	events, err := r.m.Start(ctx)
	if err != nil {
		return err
	}
	go r.run(ctx, events)
	return nil
}

func (r *Router) run(ctx context.Context, events <-chan Event) {
	defer close(r.done)
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			if h := r.handler(); h != nil {
				h.HandleMessengerEvent(ctx, evt)
			} else {
				slog.Debug("messengers: dropping event with no active session", "kind", evt.Kind)
			}
		}
	}
}

// Stop shuts down the Messenger and waits for the forwarding goroutine
// to drain.
func (r *Router) Stop(ctx context.Context) error {
	err := r.m.Stop(ctx)
	<-r.done
	return err
}
