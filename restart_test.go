package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/losinggeneration/tau/rpc/coord"
)

// restartCoordinator answers just enough of the protocol for a feed poll whose
// worker is gone, and hands the test each connection that attaches.
type restartCoordinator struct {
	mu       sync.Mutex
	items    []coord.Instance
	attached chan *coord.Link
}

func (f *restartCoordinator) setItems(items ...coord.Instance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
}

func (f *restartCoordinator) serve(conn net.Conn) {
	link := coord.NewLink(conn)
	defer func() { _ = link.Close() }()
	hello, err := link.Read()
	if err != nil {
		return
	}
	if link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, ClientID: hello.ClientID}) != nil {
		return
	}
	for {
		r, err := link.Read()
		if err != nil {
			return
		}
		reply := coord.Frame{Type: coord.TypeResponse, ID: r.ID, InstanceID: r.InstanceID}
		switch r.Type {
		case coord.TypeListInstances:
			f.mu.Lock()
			reply.Instances = f.items
			f.mu.Unlock()
		case coord.TypeCommand:
			reply.Error = &coord.Error{Code: coord.CodeUnavailable, Message: "worker is not connected"}
		}
		if link.Send(reply) != nil {
			return
		}
		if r.Type == coord.TypeAttach {
			f.attached <- link
		}
	}
}

func TestFeedFollowsRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "tau-ui-restart")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fake := &restartCoordinator{attached: make(chan *coord.Link, 4)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go fake.serve(conn)
		}
	}()
	a := app{socket: socket, restarts: newRestartWatch()}
	go a.restarts.follow(t.Context(), socket)
	poll := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/worker/"+id+"/feed?session=s", nil)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		a.feed(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("feed status = %d", w.Code)
		}
		return w
	}
	waitRestarting := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); a.restarts.isRestarting("old") != want; {
			if time.Now().After(deadline) {
				t.Fatalf("restarting never became %v", want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	a.restarts.view("old")
	var link *coord.Link
	select {
	case link = <-fake.attached:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not attach")
	}

	fake.setItems(coord.Instance{ID: "old", SessionID: "s", LinkState: coord.LinkDisconnected})
	if err := link.Send(coord.Frame{Type: coord.TypeInstanceReplacement, InstanceID: "old", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	waitRestarting(true)
	if got := poll("old").Header().Get(instanceStateHeader); got != stateRestarting {
		t.Fatalf("state during restart = %q", got)
	}

	next := coord.Instance{ID: "new", SessionID: "s", LinkState: coord.LinkConnected}
	fake.setItems(next)
	if err := link.Send(coord.Frame{Type: coord.TypeInstanceReplacement, InstanceID: "old", SessionID: "s", Instance: &next}); err != nil {
		t.Fatal(err)
	}
	waitRestarting(false)
	if got := poll("old").Header().Get(replacementHeader); got != "new" {
		t.Fatalf("replacement = %q", got)
	}

	fake.setItems()
	if got := poll("old").Header().Get(instanceStateHeader); got != stateExited {
		t.Fatalf("state after plain stop = %q", got)
	}
}

func TestReplacementFor(t *testing.T) {
	live := coord.Instance{ID: "new", SessionID: "s", LinkState: coord.LinkConnected}
	for _, c := range []struct {
		name  string
		items []coord.Instance
		next  string
		gone  bool
	}{
		{"replaced", []coord.Instance{live}, "new", true},
		{"old exited", []coord.Instance{{ID: "old", SessionID: "s", LinkState: coord.LinkExited}, live}, "new", true},
		{"old may return", []coord.Instance{{ID: "old", SessionID: "s", LinkState: coord.LinkDisconnected}, live}, "", false},
		{"other session", []coord.Instance{{ID: "new", SessionID: "other", LinkState: coord.LinkConnected}}, "", true},
		{"replacement not connected", []coord.Instance{{ID: "new", SessionID: "s", LinkState: coord.LinkDisconnected}}, "", true},
	} {
		if next, gone := replacementFor(c.items, "old", "s"); next != c.next || gone != c.gone {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, next, gone, c.next, c.gone)
		}
	}
}
