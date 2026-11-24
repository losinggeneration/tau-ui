package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/losinggeneration/tau/rpc/coord"
)

func TestDisplayNamePrefersSessionName(t *testing.T) {
	i := instance{Instance: coord.Instance{CWD: "/work/repo/main"}}
	if got := i.DisplayName(); got != "main" {
		t.Errorf("unnamed instance = %q, want the directory", got)
	}
	i.SessionName = "Auth worktree"
	if got := i.DisplayName(); got != "Auth worktree" {
		t.Errorf("named instance = %q, want the session name", got)
	}
	i.Name = "Auth agent"
	if got := i.DisplayName(); got != "Auth agent" {
		t.Errorf("renamed instance = %q, want the instance name", got)
	}
}

// nameCoordinator is a fake coordinator holding one renameable worker.
type nameCoordinator struct {
	mu        sync.Mutex
	inst      coord.Instance
	restarted *string // name sent with the restart launch
}

func (n *nameCoordinator) serve(t *testing.T, link *coord.Link) {
	defer func() { _ = link.Close() }()
	hello, err := link.Read()
	if err != nil {
		return
	}
	ops := []string{coord.OperationInstanceRename, coord.OperationInstanceRestart, coord.OperationSavedSessionLaunch}
	if link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, ClientID: hello.ClientID, Operations: ops}) != nil {
		return
	}
	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		reply := coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID}
		n.mu.Lock()
		switch f.Type {
		case coord.TypeListInstances, coord.TypeWatchInstances:
			reply.Instances = []coord.Instance{n.inst}
		case coord.TypeStopInstance:
			n.inst.LinkState = coord.LinkExited
			reply.Result = json.RawMessage(`{"type":"response","ok":true}`)
		case coord.TypeStartInstance:
			n.restarted = &f.Name
			n.inst.LinkState = coord.LinkConnected
			reply.Instance = &coord.Instance{ID: "restarted", Name: f.Name, CWD: f.CWD, SessionID: f.SessionID}
		case coord.TypeCommand:
			var cmd struct {
				Type string
				Args struct{ Name string }
			}
			if err := json.Unmarshal(f.Command, &cmd); err != nil {
				t.Error(err)
			}
			result := `{}`
			switch cmd.Type {
			case "set_instance_name":
				n.inst.Name = cmd.Args.Name
			case "get_state":
				result = `{"session_id":"session"}`
			}
			reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":` + result + `}`)
		}
		n.mu.Unlock()
		if link.Send(reply) != nil {
			return
		}
	}
}

func TestInstanceNameLifecycle(t *testing.T) {
	// t.TempDir can exceed the Unix socket path limit.
	dir, err := os.MkdirTemp("", "tau-ui-name-")
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
	fake := &nameCoordinator{inst: coord.Instance{ID: "worker", CWD: "/work/repo", SessionID: "session", SessionName: "Login fix", State: coord.StateAwaitingUser, LinkState: coord.LinkConnected}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go fake.serve(t, coord.NewLink(conn))
		}
	}()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newApp(t.Context(), socket, "test-password", dir, nil)
	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.SetBasicAuth("tau", "test-password")
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("HX-Request", "true")
			r.Header.Set("Origin", "http://example.com")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d: %s", method, path, w.Code, w.Body)
		}
		return w
	}
	rename := func(value string) *httptest.ResponseRecorder {
		t.Helper()
		return do(http.MethodPost, "/worker/worker/controls", url.Values{"action": {"instance_name"}, "value": {value}, "session": {"session"}, "tab": {"session"}})
	}

	w := rename("  Auth   agent ")
	if body := w.Body.String(); !strings.Contains(body, `value="Auth agent"`) || !strings.Contains(body, "Instance renamed.") {
		t.Errorf("rename panel: %s", body)
	}
	if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "instanceListRefresh") {
		t.Errorf("rename trigger = %q", got)
	}
	list := do(http.MethodGet, "/instances?active=worker", nil).Body.String()
	if !strings.Contains(list, `<span class="instance-name">Auth agent</span>`) || !strings.Contains(list, "Login fix") {
		t.Errorf("card should show the instance and session names: %s", list)
	}

	if !strings.Contains(list, `data-rename="Auth agent"`) {
		t.Errorf("card has no rename action: %s", list)
	}
	card := do(http.MethodPost, "/worker/worker/controls", url.Values{"action": {"instance_name"}, "inline": {"card"}, "value": {"Auth agent"}})
	if strings.Contains(card.Body.String(), "renamed") || !strings.Contains(card.Header().Get("HX-Trigger"), "instanceListRefresh") {
		t.Errorf("card rename: %q, trigger %q", card.Body, card.Header().Get("HX-Trigger"))
	}

	if w := rename(strings.Repeat("x", coord.MaxInstanceNameBytes+1)); !strings.Contains(w.Body.String(), "at most 128 bytes") {
		t.Errorf("long name: %s", w.Body)
	}
	fake.mu.Lock()
	kept := fake.inst.Name
	fake.mu.Unlock()
	if kept != "Auth agent" {
		t.Errorf("rejected rename changed the name to %q", kept)
	}

	do(http.MethodPost, "/worker/worker/lifecycle", url.Values{"op": {"restart"}, "session": {"session"}})
	fake.mu.Lock()
	restarted := fake.restarted
	fake.mu.Unlock()
	if restarted == nil || *restarted != "Auth agent" {
		t.Errorf("restart launched with name %v", restarted)
	}

	if w := rename(""); !strings.Contains(w.Body.String(), "Instance name cleared.") {
		t.Errorf("clear: %s", w.Body)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.inst.Name != "" {
		t.Errorf("name after clear = %q", fake.inst.Name)
	}
}

func TestRenameNeedsNoSession(t *testing.T) {
	for _, test := range []struct {
		name string
		inst coord.Instance
		want bool
	}{
		{"new worker", coord.Instance{ID: "w", State: coord.StateAwaitingUser, LinkState: coord.LinkConnected}, true},
		{"disconnected", coord.Instance{ID: "w", State: coord.StateAwaitingUser, LinkState: coord.LinkDisconnected}, false},
		{"exited", coord.Instance{ID: "w", LinkState: coord.LinkExited}, false},
	} {
		var out strings.Builder
		if err := pages.ExecuteTemplate(&out, "instance-card", summary(test.inst)); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), "data-rename="); got != test.want {
			t.Errorf("%s: rename shown = %t", test.name, got)
		}
		if strings.Contains(out.String(), `value="stop"`) {
			t.Errorf("%s: Stop needs a session", test.name)
		}
	}
}
