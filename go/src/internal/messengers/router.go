package messengers

import (
	"context"
	"log/slog"
	"sync"
)

type EventHandler interface {
	HandleMessengerEvent(ctx context.Context, evt Event)
}

// Router owns the long-lived Messenger and forwards its events to a swappable
// handler. The messenger runs for the whole daemon (so /register works before
// any meeting); each session installs itself as the handler while active.
// Events that arrive with no handler are dropped — a registration is already
// persisted, and confirmations/answers outside a session aren't actionable.
type Router struct {
	m    Messenger
	done chan struct{}

	mu sync.RWMutex
	h  EventHandler
}

func NewRouter(m Messenger) *Router {
	return &Router{m: m, done: make(chan struct{})}
}

func (r *Router) Messenger() Messenger { return r.m }

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

func (r *Router) Stop(ctx context.Context) error {
	err := r.m.Stop(ctx)
	<-r.done
	return err
}
