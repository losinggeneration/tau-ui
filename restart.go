package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
)

const (
	// restartWait matches how long Tau remembers a restart's viewers.
	restartWait = time.Minute
	// viewerIdle ends a subscription once no page has polled the instance.
	viewerIdle = time.Minute

	replacementHeader   = "X-Tau-Replacement"
	instanceStateHeader = "X-Tau-Instance-State"
	stateRestarting     = "restarting"
	stateExited         = "exited"
)

// restartWatch learns about restarts for instances open in a browser. Tau only
// announces them to attached clients, and page requests are too short-lived to
// stay attached. It has its own connection because attaching receives every
// event, and falling behind must not disturb the inbox watcher.
type restartWatch struct {
	mu         sync.Mutex
	viewed     map[string]time.Time // last feed poll
	restarting map[string]time.Time // announcement expiry
	wake       chan struct{}
}

func newRestartWatch() *restartWatch {
	return &restartWatch{
		viewed:     make(map[string]time.Time),
		restarting: make(map[string]time.Time),
		wake:       make(chan struct{}, 1),
	}
}

// view records a successful feed poll and subscribes to a newly opened instance.
func (t *restartWatch) view(id string) {
	t.mu.Lock()
	_, known := t.viewed[id]
	t.viewed[id] = time.Now()
	t.mu.Unlock()
	if !known {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

func (t *restartWatch) follow(ctx context.Context, socket string) {
	for ctx.Err() == nil {
		t.watch(ctx, socket)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (t *restartWatch) watch(parent context.Context, socket string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connect, stop := context.WithTimeout(ctx, 2*time.Second)
	c, err := client.NewCoordinatedClient(connect, socket)
	stop()
	if err != nil {
		return // Feed polls report if Tau can't be reached.
	}
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("close Tau restart watcher: %v", err)
		}
	}()
	attached := make(map[string]bool)
	failed := make(chan string)
	request := func(id string, fn func(context.Context, string) error, report bool) {
		call, done := context.WithTimeout(ctx, 4*time.Second)
		defer done()
		if err := fn(call, id); err != nil && report {
			select {
			case failed <- id:
			case <-ctx.Done():
			}
		}
	}
	reconcile := func() {
		now := time.Now()
		t.mu.Lock()
		defer t.mu.Unlock()
		for id, seen := range t.viewed {
			switch {
			case now.Sub(seen) > viewerIdle:
				delete(t.viewed, id)
				if attached[id] {
					delete(attached, id)
					go request(id, c.Detach, false)
				}
			case !attached[id]:
				attached[id] = true
				go request(id, c.Attach, true)
			}
		}
		for id, expires := range t.restarting {
			if now.After(expires) {
				delete(t.restarting, id)
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(viewerIdle / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.wake:
			reconcile()
		case <-ticker.C:
			reconcile()
		case id := <-failed:
			// The next successful poll subscribes again.
			delete(attached, id)
			t.mu.Lock()
			delete(t.viewed, id)
			t.mu.Unlock()
		case f, ok := <-c.Events():
			if !ok {
				return
			}
			if f.Type != coord.TypeInstanceReplacement {
				continue
			}
			t.mu.Lock()
			if f.Instance == nil {
				t.restarting[f.InstanceID] = time.Now().Add(restartWait)
			} else {
				delete(t.restarting, f.InstanceID)
				delete(t.viewed, f.InstanceID)
				delete(attached, f.InstanceID)
			}
			t.mu.Unlock()
		}
	}
}

func (t *restartWatch) isRestarting(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	expires, ok := t.restarting[id]
	return ok && time.Now().Before(expires)
}

// fate explains a failed feed poll. The replacement comes from the instance
// list, not the announcement, so a tab that was hidden or connected too late
// to hear it still lands on the running session.
func (t *restartWatch) fate(ctx context.Context, c *client.CoordinatedClient, id, session string) (next, state string) {
	restarting := t.isRestarting(id)
	if restarting {
		state = stateRestarting
	}
	items, err := c.ListInstances(ctx)
	if err != nil {
		return "", state
	}
	next, gone := replacementFor(items, id, session)
	if next != "" {
		return next, ""
	}
	if !restarting && gone {
		state = stateExited
	}
	return "", state
}

// replacementFor finds the connected worker now running session. gone reports
// that id has exited or left the list. A worker that is only disconnected may
// come back under the same ID, so it isn't replaced.
func replacementFor(items []coord.Instance, id, session string) (next string, gone bool) {
	gone = true
	for _, i := range items {
		if i.ID == id {
			gone = i.LinkState == coord.LinkExited
		} else if session != "" && i.SessionID == session && i.LinkState == coord.LinkConnected {
			next = i.ID
		}
	}
	if !gone {
		return "", false
	}
	return next, true
}
