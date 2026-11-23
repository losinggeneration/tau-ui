package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
)

type inboxStatus struct {
	badge, session, link string
}

func statusOf(i coord.Instance) inboxStatus {
	return inboxStatus{badge: summary(i).Badge, session: i.SessionID, link: i.LinkState}
}

type inboxEntry struct {
	status inboxStatus
	since  time.Time
}

// inboxOrder watches worker transitions so a short run between two sidebar
// polls still moves its worker to the top when it becomes ready again.
type inboxOrder struct {
	mu         sync.Mutex
	generation string
	entries    map[string]inboxEntry
	// dismissed is keyed by session so it survives worker and Tau UI restarts.
	dismissed     map[string]limitDismissal
	dismissedPath string
}

// limitDismissal records that the user moved a session's usage limit to Waiting.
type limitDismissal struct {
	Provider string    `json:"provider"`
	At       time.Time `json:"at"`
}

// dismissalTTL drops records for sessions Tau no longer reports as limited.
// It only needs to outlast a weekly provider quota and a long daemon outage.
const dismissalTTL = 30 * 24 * time.Hour

func dismissalOwner(i coord.Instance) string { return cmp.Or(i.SessionID, i.ID) }

// limitKey names a session's wait on a provider. It leaves out the reset time
// because Tau publishes a new one each time a retry hits the same limit.
func limitKey(i coord.Instance) string {
	if i.State != coord.StateUsageLimited {
		return ""
	}
	return dismissalOwner(i) + "/" + i.StateDetail.Provider
}

// endsDismissal reports whether a connected worker has finished its run. Other
// states can be a blip inside the run: Tau reports working while it retries a
// limit, and a disconnected worker's state is stale.
func endsDismissal(i coord.Instance) bool {
	if i.LinkState != "" && i.LinkState != coord.LinkConnected {
		return false
	}
	return i.State == coord.StateAwaitingUser || i.State == coord.StateError
}

// dismissedLimits returns the workers sitting on a dismissed limit and forgets
// dismissals whose run has ended.
func (o *inboxOrder) dismissedLimits(items []coord.Instance) map[string]bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	active := make(map[string]bool, len(o.dismissed))
	limited := make(map[string]bool, len(o.dismissed))
	changed := false
	for _, i := range items {
		owner := dismissalOwner(i)
		d, ok := o.dismissed[owner]
		switch {
		case !ok:
		case endsDismissal(i), i.State == coord.StateUsageLimited && i.StateDetail.Provider != d.Provider:
			delete(o.dismissed, owner)
			changed = true
		case i.State == coord.StateUsageLimited:
			active[i.ID], limited[owner] = true, true
		}
	}
	for owner, d := range o.dismissed {
		if !limited[owner] && time.Since(d.At) > dismissalTTL {
			delete(o.dismissed, owner)
			changed = true
		}
	}
	if changed {
		o.saveDismissed()
	}
	return active
}

func (o *inboxOrder) dismissLimit(i coord.Instance) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dismissed == nil {
		o.dismissed = make(map[string]limitDismissal)
	}
	o.dismissed[dismissalOwner(i)] = limitDismissal{Provider: i.StateDetail.Provider, At: time.Now().UTC()}
	o.saveDismissed()
}

// dismissalsPath follows the XDG state directory convention.
func dismissalsPath() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "tau-ui", "dismissed-limits.json"), nil
}

// loadDismissed restores dismissals. Without a readable file the UI still
// works, and limits alert again.
func (o *inboxOrder) loadDismissed(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dismissedPath = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(data, &o.dismissed)
	}
	if err != nil {
		o.dismissed = nil
		log.Printf("load dismissed usage limits: %v", err)
	}
}

// saveDismissed replaces the file atomically. Caller holds mu.
func (o *inboxOrder) saveDismissed() {
	if o.dismissedPath == "" {
		return
	}
	data, err := json.Marshal(o.dismissed)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(o.dismissedPath), 0o700)
	}
	if err == nil {
		err = os.WriteFile(o.dismissedPath+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(o.dismissedPath+".tmp", o.dismissedPath)
	}
	if err != nil {
		log.Printf("save dismissed usage limits: %v", err)
	}
}

// dismissLimit moves a usage-limited worker to Waiting. A stale card names a
// limit that has ended, so it changes nothing and the list refreshes.
func (a app) dismissLimit(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	id, limit := r.PathValue("id"), r.PostForm.Get("limit")
	err := a.rpc(r, 4*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		items, err := c.ListInstances(ctx)
		if err != nil {
			return err
		}
		at := slices.IndexFunc(items, func(i coord.Instance) bool { return i.ID == id })
		if at >= 0 && limit != "" && limitKey(items[at]) == limit {
			a.inbox.dismissLimit(items[at])
		}
		return nil
	})
	if err != nil {
		result(w, "", err)
		return
	}
	w.Header().Set("HX-Trigger-After-Settle", "instanceListRefresh")
	w.WriteHeader(http.StatusOK)
}

func (o *inboxOrder) apply(i coord.Instance, now time.Time) {
	status := statusOf(i)
	if old, ok := o.entries[i.ID]; !ok || old.status != status {
		o.entries[i.ID] = inboxEntry{status: status, since: now}
	}
}

func (o *inboxOrder) baseline(generation string, items []coord.Instance) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.generation = generation
	o.entries = make(map[string]inboxEntry, len(items))
	for _, i := range items {
		o.entries[i.ID] = inboxEntry{status: statusOf(i), since: i.LastActivity}
	}
}

func (o *inboxOrder) sortTimes(generation string, items []coord.Instance) map[string]time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	times := make(map[string]time.Time, len(items))
	for _, i := range items {
		times[i.ID] = i.LastActivity
		if entry, ok := o.entries[i.ID]; ok && o.generation == generation && entry.status == statusOf(i) {
			times[i.ID] = entry.since
		}
	}
	return times
}

func (o *inboxOrder) follow(ctx context.Context, socket string) {
	for ctx.Err() == nil {
		o.watch(ctx, socket)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (o *inboxOrder) watch(ctx context.Context, socket string) {
	connect, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := client.NewCoordinatedClient(connect, socket)
	if err != nil {
		return // HTTP requests report if Tau can't be reached.
	}
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("close Tau instance watcher: %v", err)
		}
	}()
	items, err := c.WatchInstances(connect)
	if err != nil {
		return // Fall back to last activity while the watcher is unavailable.
	}
	o.baseline(c.Generation(), items)
	defer o.baseline("", nil)
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-c.Events():
			if !ok {
				return
			}
			if f.Type != coord.TypeInstanceUpdate {
				continue
			}
			o.mu.Lock()
			if f.Instance == nil {
				delete(o.entries, f.InstanceID)
			} else {
				o.apply(*f.Instance, time.Now().UTC())
			}
			o.mu.Unlock()
		}
	}
}
