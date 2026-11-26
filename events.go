package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	eventInstances    = "instances"
	eventConversation = "conversation"

	// eventGap bounds how often one page refetches. Streaming output sends an
	// event per chunk, and each event only means "refetch".
	eventGap = 250 * time.Millisecond
	// eventPing keeps proxies from closing a quiet stream.
	eventPing = 20 * time.Second
)

// eventHub tells open pages that something changed so they refetch. Events
// carry no data, so a page that falls behind only needs the latest of each.
type eventHub struct {
	mu   sync.Mutex
	subs map[*eventSub]struct{}
}

type eventSub struct {
	worker  string
	mu      sync.Mutex
	pending map[string]bool
	wake    chan struct{}
}

func newEventHub() *eventHub { return &eventHub{subs: make(map[*eventSub]struct{})} }

// publish sends name to every page, or only to pages showing worker.
func (h *eventHub) publish(name, worker string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if worker != "" && s.worker != worker {
			continue
		}
		s.mu.Lock()
		s.pending[name] = true
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (h *eventHub) subscribe(worker string) *eventSub {
	s := &eventSub{worker: worker, pending: make(map[string]bool), wake: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *eventHub) unsubscribe(s *eventSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (s *eventSub) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.pending))
	for name := range s.pending {
		names = append(names, name)
	}
	clear(s.pending)
	return names
}

func (a app) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// The stream outlives any server write timeout.
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	worker := r.URL.Query().Get("worker")
	s := a.hub.subscribe(worker)
	defer a.hub.unsubscribe(s)
	// Tau only sends a worker's events to attached clients, so keep the
	// restart watcher attached while this page is open.
	view := func() {
		if worker != "" && a.restarts != nil {
			a.restarts.view(worker)
		}
	}
	view()
	ping := time.NewTicker(eventPing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			view()
			_, _ = fmt.Fprint(w, ": ping\n\n")
		case <-s.wake:
			for _, name := range s.take() {
				_, _ = fmt.Fprintf(w, "event: %s\ndata:\n\n", name)
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(eventGap):
		}
	}
}
