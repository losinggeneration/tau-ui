package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/losinggeneration/tau/rpc/coord"
)

func TestInstanceGroups(t *testing.T) {
	reset := time.Date(2026, 11, 24, 14, 0, 0, 0, time.Local)
	limit := coord.StateDetail{Provider: "codex", ResetsAt: reset}
	for _, test := range []struct {
		name, group, label string
		item               coord.Instance
		dismissed          bool
	}{
		{name: "error", group: "attention", label: "error: boom", item: coord.Instance{State: coord.StateError, StateDetail: coord.StateDetail{Message: "boom"}}},
		{name: "usage limit", group: "attention", label: "usage limit: codex, resets Tue Nov 24 14:00", item: coord.Instance{State: coord.StateUsageLimited, StateDetail: limit}},
		{name: "dismissed usage limit", group: "waiting", label: "usage limit: codex, resets Tue Nov 24 14:00", item: coord.Instance{State: coord.StateUsageLimited, StateDetail: limit}, dismissed: true},
		{name: "retired ready", group: "attention", label: "unknown: ready", item: coord.Instance{State: "ready"}},
		{name: "retired busy", group: "attention", label: "unknown: busy", item: coord.Instance{State: "busy"}},
		{name: "missing state", group: "attention", label: "unknown"},
		{name: "awaiting user", group: "input", label: "awaiting user", item: coord.Instance{State: coord.StateAwaitingUser}},
		{name: "needs input", group: "input", label: "needs input", item: coord.Instance{State: coord.StateNeedsInput}},
		{name: "pending dialogs", group: "input", label: "needs input (2)", item: coord.Instance{State: coord.StateWorking, PendingInputs: 2}},
		{name: "working", group: "working", label: "working", item: coord.Instance{State: coord.StateWorking}},
		{name: "retrying", group: "working", label: "retrying (2/5)", item: coord.Instance{State: coord.StateRetrying, StateDetail: coord.StateDetail{Attempt: 2, MaxAttempts: 5}}},
		{name: "paused", group: "waiting", label: "paused: codex", item: coord.Instance{State: coord.StatePaused, StateDetail: coord.StateDetail{Target: "codex"}}},
		{name: "disconnected", group: "disconnected", label: "disconnected", item: coord.Instance{State: coord.StateError, LinkState: coord.LinkDisconnected}},
		{name: "exited", group: "done", label: "exited", item: coord.Instance{State: "busy", LinkState: coord.LinkExited}},
	} {
		t.Run(test.name, func(t *testing.T) {
			card := summary(test.item)
			card.Dismissed = test.dismissed
			groups := groupInstances([]instance{card})
			if len(groups) != 1 || groups[0].Key != test.group || card.Label != test.label {
				t.Fatalf("got groups %+v, label %q; want group %q, label %q", groups, card.Label, test.group, test.label)
			}
		})
	}
}

func TestDismissedLimitHoldsForTheRun(t *testing.T) {
	limited := coord.Instance{ID: "worker", SessionID: "session", State: coord.StateUsageLimited, StateDetail: coord.StateDetail{Provider: "codex", ResetsAt: time.Unix(100, 0)}}
	with := func(change func(*coord.Instance)) coord.Instance {
		i := limited
		change(&i)
		return i
	}
	dismissed := func(o *inboxOrder, i coord.Instance) bool { return o.dismissedLimits([]coord.Instance{i})[i.ID] }
	for _, test := range []struct {
		name  string
		seen  []coord.Instance
		alert bool
	}{
		{name: "new reset time", seen: []coord.Instance{with(func(i *coord.Instance) { i.StateDetail.ResetsAt = time.Unix(200, 0) })}},
		{name: "retry blip", seen: []coord.Instance{with(func(i *coord.Instance) { i.State, i.StateDetail = coord.StateWorking, coord.StateDetail{} })}},
		{name: "dialog", seen: []coord.Instance{with(func(i *coord.Instance) { i.State, i.StateDetail = coord.StateNeedsInput, coord.StateDetail{} })}},
		{name: "link drop", seen: []coord.Instance{with(func(i *coord.Instance) { i.State, i.LinkState = coord.StateAwaitingUser, coord.LinkDisconnected })}},
		{name: "missing from list", seen: []coord.Instance{{ID: "other", State: coord.StateWorking}}},
		{name: "new worker on the session", seen: []coord.Instance{with(func(i *coord.Instance) { i.ID = "replacement" })}},
		{name: "run finished", seen: []coord.Instance{with(func(i *coord.Instance) { i.State, i.StateDetail = coord.StateAwaitingUser, coord.StateDetail{} })}, alert: true},
		{name: "run failed", seen: []coord.Instance{with(func(i *coord.Instance) { i.State, i.StateDetail = coord.StateError, coord.StateDetail{} })}, alert: true},
		{name: "other provider", seen: []coord.Instance{with(func(i *coord.Instance) { i.StateDetail.Provider = "claude" })}, alert: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			order := new(inboxOrder)
			order.dismissLimit(limited)
			for _, seen := range test.seen {
				order.dismissedLimits([]coord.Instance{seen})
			}
			if got := dismissed(order, limited); got == test.alert {
				t.Fatalf("dismissed = %v, want %v", got, !test.alert)
			}
		})
	}
}

func TestDismissedLimitSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "dismissed-limits.json")
	limited := coord.Instance{ID: "worker", SessionID: "session", State: coord.StateUsageLimited, StateDetail: coord.StateDetail{Provider: "codex", ResetsAt: time.Unix(100, 0)}}
	before := new(inboxOrder)
	before.loadDismissed(path)
	before.dismissLimit(limited)

	after := new(inboxOrder)
	after.loadDismissed(path)
	if !after.dismissedLimits([]coord.Instance{limited})[limited.ID] {
		t.Fatal("expected the dismissal to survive a restart")
	}
	finished := limited
	finished.State, finished.StateDetail = coord.StateAwaitingUser, coord.StateDetail{}
	after.dismissedLimits([]coord.Instance{finished})

	restarted := new(inboxOrder)
	restarted.loadDismissed(path)
	if restarted.dismissedLimits([]coord.Instance{limited})[limited.ID] {
		t.Fatal("expected a finished run's dismissal to stay forgotten after a restart")
	}
}

func TestDismissalExpiresOnlyWhenNotLimited(t *testing.T) {
	limited := coord.Instance{ID: "worker", SessionID: "session", State: coord.StateUsageLimited, StateDetail: coord.StateDetail{Provider: "codex"}}
	old := time.Now().Add(-2 * dismissalTTL)
	order := &inboxOrder{dismissed: map[string]limitDismissal{"session": {Provider: "codex", At: old}, "gone": {Provider: "codex", At: old}}}
	if !order.dismissedLimits([]coord.Instance{limited})[limited.ID] {
		t.Fatal("a long wait should stay dismissed")
	}
	if _, ok := order.dismissed["gone"]; ok {
		t.Fatal("expected an old record for a missing session to expire")
	}
}

func TestInboxKeepsReadyTransitionTime(t *testing.T) {
	older := coord.Instance{ID: "older", State: coord.StateAwaitingUser, LastActivity: time.Unix(10, 0)}
	newer := coord.Instance{ID: "newer", State: coord.StateAwaitingUser, LastActivity: time.Unix(20, 0)}
	order := new(inboxOrder)
	order.baseline("daemon", []coord.Instance{older, newer})

	// A whole run can happen between two reads of the ready list.
	older.State = coord.StateWorking
	order.apply(older, time.Unix(30, 0))
	older.State = coord.StateAwaitingUser
	becameReady := time.Unix(40, 0)
	order.apply(older, becameReady)

	// A ready worker can publish activity without becoming ready again.
	newer.LastActivity = time.Unix(50, 0)
	order.apply(newer, newer.LastActivity)
	items := []coord.Instance{older, newer}
	times := order.sortTimes("daemon", items)
	if !times[older.ID].Equal(becameReady) || !times[older.ID].After(times[newer.ID]) {
		t.Fatalf("expected the worker that just became ready to outrank unrelated activity: %v", times)
	}
	if times := order.sortTimes("new-daemon", items); !times[newer.ID].Equal(newer.LastActivity) {
		t.Fatalf("expected last activity as a fallback after a daemon restart: %v", times)
	}
}
