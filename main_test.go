package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
)

func TestCACertificateDownload(t *testing.T) {
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, cert, 0600); err != nil {
		t.Fatal(err)
	}
	ca, err := readCACertificate(path)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	if err := os.WriteFile(path, bytes.Join([][]byte{cert, keyPEM}, nil), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCACertificate(path); err == nil {
		t.Error("CA file containing a private key was accepted")
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newApp(t.Context(), filepath.Join(dir, "unused.sock"), "test-password", dir, ca)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ca.crt", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), cert) ||
		w.Header().Get("Content-Disposition") != `attachment; filename="tau-ui-ca.crt"` {
		t.Errorf("CA download: %d, disposition %q", w.Code, w.Header().Get("Content-Disposition"))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("UI without credentials: got %d, want 401", w.Code)
	}
}

func TestProjectDirSearchLimit(t *testing.T) {
	root := t.TempDir()
	for i := range projectSearchLimit + 1 {
		if err := os.MkdirAll(filepath.Join(root, "projects", fmt.Sprintf("candidate-%04d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	a := app{projectRoot: root}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"candidate", projectSearchLimit - 1}, // The immediate child also consumes one slot.
		{"no match", 0},
	} {
		v, err := a.readProjectDirs(t.Context(), ".", tc.query, projectDirOptions{})
		if err != nil || !v.Limited || len(v.Dirs) != tc.want {
			t.Fatalf("search %q: got %d matches, limited=%v, err=%v; want %d matches and limit reached", tc.query, len(v.Dirs), v.Limited, err, tc.want)
		}
	}
}

func TestProjectDirSearchOrder(t *testing.T) {
	mkdir := func(t *testing.T, parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(parts...), 0700); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(t *testing.T, path string, at time.Time) {
		t.Helper()
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	found := func(t *testing.T, root, query, want string) bool {
		t.Helper()
		v, err := app{projectRoot: root}.readProjectDirs(t.Context(), ".", query, projectDirOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(v.Dirs, func(d projectDir) bool { return d.Name == want })
	}
	now := time.Now()

	t.Run("recent folders spend the budget first", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, root, "a-stale", "wanted")
		for i := range projectSearchLimit {
			mkdir(t, root, "recent-00", fmt.Sprintf("fill-%04d", i))
		}
		for i := range projectSearchRecent {
			name := fmt.Sprintf("recent-%02d", i)
			mkdir(t, root, name)
			touch(t, filepath.Join(root, name), now.Add(-time.Duration(i)*time.Minute))
		}
		touch(t, filepath.Join(root, "a-stale"), now.Add(-time.Hour))
		if found(t, root, "wanted", "a-stale/wanted") {
			t.Error("stale folder was searched before the recent folders")
		}
	})

	t.Run("every top-level folder reaches depth three before any goes deeper", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, root, "old", "one", "two", "wanted")
		for i := range projectSearchLimit {
			mkdir(t, root, "new", "one", "two", "three", fmt.Sprintf("fill-%04d", i))
		}
		touch(t, filepath.Join(root, "old"), now.Add(-time.Hour))
		touch(t, filepath.Join(root, "new"), now)
		if !found(t, root, "wanted", filepath.Join("old", "one", "two", "wanted")) {
			t.Error("deep scan of a recent folder starved a shallower folder")
		}
	})

	t.Run("search continues to full depth and skips hidden folders", func(t *testing.T) {
		root := t.TempDir()
		deep := filepath.Join("a", "b", "c", "d", "e", "f", "wanted")
		mkdir(t, root, deep)
		mkdir(t, root, ".hidden", "wanted")
		mkdir(t, root, "a", ".hidden", "wanted")
		v, err := app{projectRoot: root}.readProjectDirs(t.Context(), ".", "wanted", projectDirOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Dirs) != 1 || v.Dirs[0].Name != deep || v.Limited {
			t.Errorf("got %+v, limited=%v; want only %q", v.Dirs, v.Limited, deep)
		}
	})
}

func TestProjectDirSearchOptions(t *testing.T) {
	root := t.TempDir()
	want := map[string]projectDirOptions{
		"app/match":              {},
		"app/node_modules/match": {Deps: true},
		"app/vendor/match":       {Deps: true},
		"app/target/match":       {Deps: true},
		".config/match":          {Hidden: true},
		".config/vendor/match":   {Hidden: true, Deps: true},
	}
	for path := range want {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, opts := range []projectDirOptions{{}, {Hidden: true}, {Deps: true}, {Hidden: true, Deps: true}} {
		v, err := app{projectRoot: root}.readProjectDirs(t.Context(), ".", "match", opts)
		if err != nil {
			t.Fatal(err)
		}
		var got, expected []string
		for _, dir := range v.Dirs {
			got = append(got, dir.Name)
		}
		for path, needs := range want {
			if (opts.Hidden || !needs.Hidden) && (opts.Deps || !needs.Deps) {
				expected = append(expected, path)
			}
		}
		slices.Sort(expected)
		if !slices.Equal(got, expected) {
			t.Errorf("options %+v: got %v, want %v", opts, got, expected)
		}
	}
}

func TestHTTPToCoordinator(t *testing.T) {
	dir, err := os.MkdirTemp("", "tau-ui-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan json.RawMessage, 16)
	var modelJSON atomic.Pointer[string]
	modelJSON.Store(new(`"anthropic/claude-opus-5-5"`))
	const defaultQueue = `{"steering":0,"follow_up":1,"items":[{"id":"queued-1","kind":"follow_up","editable":true,"text":"A pending follow-up"}]}`
	var queueJSON atomic.Pointer[string]
	queueJSON.Store(new(defaultQueue))
	var clients sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer clients.Wait()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				return
			}
			clients.Go(func() { fakeRPC(t, coord.NewLink(conn), commands, &modelJSON, &queueJSON) })
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		<-done
	})
	projectRoot := filepath.Join(dir, "projects")
	projectName := "A & B+ #%' "
	project := filepath.Join(projectRoot, projectName)
	for _, folder := range []string{filepath.Join(project, "nested", "Deeper"), filepath.Join(project, "empty"), filepath.Join(projectRoot, ".hidden", "nested"), filepath.Join(dir, "outside", "private")} {
		if err := os.MkdirAll(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "not-a-folder"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(projectRoot, "shortcut")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newApp(t.Context(), socket, "test-password", projectRoot, nil)
	request := func(method, path string, auth bool, origin string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		if auth {
			r.SetBasicAuth("tau", "test-password")
		}
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("HX-Request", "true")
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, check := range []struct {
		path, want string
	}{
		{"/", "Needs input"},
		{"/", `<link rel="manifest" href="/app.webmanifest" crossorigin="use-credentials">`},
		{"/", `hx-get="/project-dirs"`},
		{"/project-dirs", `hx-get="/project-dirs?dir=` + url.QueryEscape(projectName) + `"`},
		{"/project-dirs?dir=" + url.QueryEscape(projectName), `data-project-path="` + project + `"`},
		{"/project-dirs?dir=" + url.QueryEscape(projectName), "Up one folder"},
		{"/project-dirs?dir=" + url.QueryEscape(projectName), "nested"},
		{"/project-dirs?dir=" + url.QueryEscape(projectName+"/empty"), "No subfolders to browse."},
		{"/project-dirs?dir=" + url.QueryEscape(projectName+"/missing"), "Can't browse this folder"},
		{"/project-dirs?q=NeStEd", projectName + "/nested"},
		{"/project-dirs?q=NeStEd", "1 match"},
		{"/project-dirs?q=Deeper", `hx-get="/project-dirs?dir=` + url.QueryEscape(projectName+"/nested/Deeper") + `"`},
		{"/project-dirs?q=no-such-folder", "No matching folders."},
		{"/project-dirs?dir=" + url.QueryEscape(projectName) + "&q=Deeper", `hx-get="/project-dirs?dir=` + url.QueryEscape(projectName+"/nested/Deeper") + `"`},
		{"/project-dirs?dir=" + url.QueryEscape(projectName) + "&q=private", "No matching folders."},
		{"/project-dirs?q=not-a-folder", "No matching folders."},
		{"/project-dirs?dir=..%2Foutside&q=private", "Can't browse this folder"},
		{"/project-dirs?dir=..%2Foutside&hidden=1", "Can't browse this folder"},
		{"/project-dirs?dir=.hidden", "Can't browse this folder"},
		{"/project-dirs?q=nested&hidden=1", `hx-get="/project-dirs?dir=.hidden%2Fnested&hidden=1"`},
		{"/project-dirs?dir=.hidden&hidden=1", `data-project-path="` + filepath.Join(projectRoot, ".hidden") + `"`},
		{"/project-dirs?dir=.hidden&hidden=1", `<input type="hidden" name="hidden" value="1">`},
		{"/project-dirs?dir=.hidden%2Fnested&hidden=1&deps=1", `hx-get="/project-dirs?dir=.hidden&hidden=1&deps=1"`},
		{"/instances?active=worker", `aria-current="page"`},
		{"/instances", `aria-label="Stop project"`},
		{"/instances", `aria-label="Restart project"`},
		{"/worker/worker", "Load older messages"},
		{"/worker/worker/feed?session=session", `data-ready="true"`},
		{"/worker/worker/feed?session=old", "switched sessions"},
		{"/worker/worker/history?session=session&before=previous", "Older prompt"},
		{"/worker/worker/feed?session=session", `data-status="Running"`},
		{"/htmx.min.js", "htmx"},
		{"/code.css", ".chroma"},
	} {
		w := request(http.MethodGet, check.path, true, "", nil)
		if w.Code != 200 || !strings.Contains(html.UnescapeString(w.Body.String()), check.want) {
			t.Fatalf("GET %s: %d, missing %q: %.1500s", check.path, w.Code, check.want, w.Body.String())
		}
	}
	manifest := request(http.MethodGet, "/app.webmanifest", true, "", nil)
	if manifest.Code != http.StatusOK || manifest.Header().Get("Content-Type") != "application/manifest+json" ||
		!strings.Contains(manifest.Body.String(), `"display": "standalone"`) {
		t.Fatalf("GET manifest: %d %s: %s", manifest.Code, manifest.Header().Get("Content-Type"), manifest.Body.String())
	}
	for _, path := range []string{"/icon-192.png", "/icon-512.png"} {
		w := request(http.MethodGet, path, true, "", nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
			t.Errorf("GET %s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	if w := request(http.MethodGet, "/project-dirs", false, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated folder browse: %d", w.Code)
	}
	rootHTML := request(http.MethodGet, "/project-dirs", true, "", nil).Body.String()
	for _, name := range []string{".hidden", "not-a-folder", "shortcut", "Up one folder"} {
		if strings.Contains(rootHTML, name) {
			t.Errorf("unexpected %q at browse root", name)
		}
	}
	for _, invalid := range []string{"../outside", filepath.Join(dir, "outside"), "shortcut", ".hidden", "not-a-folder"} {
		w := request(http.MethodGet, "/project-dirs?dir="+url.QueryEscape(invalid), true, "", nil)
		if !strings.Contains(html.UnescapeString(w.Body.String()), "Can't browse this folder") || strings.Contains(w.Body.String(), "data-project-path=") {
			t.Errorf("unexpected browse response for %q: %s", invalid, w.Body.String())
		}
	}
	if w := request(http.MethodPost, "/start", true, "http://example.com", url.Values{"cwd": {project}}); w.Header().Get("HX-Redirect") != "/worker/started" {
		t.Fatalf("start picked project: redirect %q, body %s", w.Header().Get("HX-Redirect"), w.Body.String())
	}
	select {
	case raw := <-commands:
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != project {
			t.Fatalf("start cwd: got %q, want %q (decode: %v)", got, project, err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing coordinator launch")
	}
	if list := request(http.MethodGet, "/instances", true, "", nil).Body.String(); strings.Contains(list, `hx-confirm="Restart`) || !strings.Contains(list, `data-busy-label="Restarting…"`) {
		t.Error("restart should run without a confirmation dialog and show progress on the card")
	}
	if list := request(http.MethodGet, "/instances", true, "", nil).Body.String(); strings.Contains(list, `hx-sync="#instance-list:drop"`) || !strings.Contains(list, `hx-sync="closest .instance-card:drop"`) || !strings.Contains(list, `hx-swap="beforeend"`) {
		t.Error("card actions should only block other actions on the same card and keep each other's errors")
	}
	inbox := request(http.MethodGet, "/instances?active=ready-new", true, "", nil).Body.String()
	previousIndex := -1
	for _, marker := range []string{`data-instance-group="attention"`, `href="/worker/limited"`,
		`data-instance-group="input"`, `href="/worker/ready-new"`, `href="/worker/ready-old"`, `href="/worker/worker"`,
		`<details class="instance-group" data-instance-group="working" open>`, `href="/worker/busy"`,
		`<details class="instance-group" data-instance-group="done">`, `Exited project`,
	} {
		if index := strings.Index(inbox, marker); index <= previousIndex {
			t.Fatalf("inbox missing or out of order at %q: %s", marker, inbox)
		} else {
			previousIndex = index
		}
	}
	if strings.Contains(inbox, `/worker/exited`) {
		t.Fatal("exited worker shouldn't link to an unavailable conversation")
	}
	if strings.Contains(inbox, `data-instance-group="waiting"`) || strings.Contains(inbox, `data-instance-group="disconnected"`) {
		t.Fatal("empty groups should be hidden")
	}
	dismiss := func(limit string) string {
		t.Helper()
		if w := request(http.MethodPost, "/worker/limited/dismiss-limit", true, "http://example.com", url.Values{"limit": {limit}}); w.Code != http.StatusOK {
			t.Fatalf("dismiss: %d %s", w.Code, w.Body.String())
		}
		return request(http.MethodGet, "/instances", true, "", nil).Body.String()
	}
	if list := dismiss("an-ended-limit"); !strings.Contains(list, `data-instance-group="attention"`) {
		t.Fatal("a stale dismissal shouldn't move the current limit")
	}
	list := dismiss(limitKey(testLimited))
	workingAt, waitingAt := strings.Index(list, `data-instance-group="working"`), strings.Index(list, `data-instance-group="waiting"`)
	if strings.Contains(list, `data-instance-group="attention"`) || workingAt < 0 || waitingAt < workingAt || strings.Index(list, `href="/worker/limited"`) < waitingAt || strings.Contains(list, `/worker/limited/dismiss-limit`) {
		t.Fatalf("dismissed limit should sit in Waiting below Working: %s", list)
	}
	w := request(http.MethodGet, "/worker/worker", true, "", nil)
	for _, want := range []string{"<strong>Bold</strong>", `class="chroma"`, "Partial reply", "Approve changes", "notes.txt", "Reading lines", "Queued messages", "A pending follow-up", `data-queue-edit="queued-1"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("conversation missing %q", want)
		}
	}
	if got := strings.Count(w.Body.String(), `data-role="user"`); got != 1 || !strings.Contains(w.Body.String(), `class="message-image"`) || strings.Contains(w.Body.String(), "[image content]") || strings.Contains(w.Body.String(), "[Image]") {
		t.Errorf("expected one user bubble with its image, got %d user rows", got)
	}
	if !strings.Contains(w.Body.String(), "Show full content") || strings.Contains(w.Body.String(), "too large to show") {
		t.Error("truncated live message should offer its full content instead of a notice")
	}
	expanded := request(http.MethodGet, "/worker/worker/content?session=session&entry=reply&live=message", true, "", nil)
	if expanded.Code != 200 || !strings.Contains(expanded.Body.String(), "Start of reply. Partial reply") || !strings.Contains(expanded.Body.String(), "data-expanded") {
		t.Errorf("full live message: %d %s", expanded.Code, expanded.Body.String())
	}
	image := request(http.MethodGet, "/worker/worker/content?session=session&entry=photo&part=image&index=0", true, "", nil)
	if image.Code != 200 || image.Header().Get("Content-Type") != "image/png" || !bytes.Equal(image.Body.Bytes(), testPNG) {
		t.Errorf("inline image: %d %s", image.Code, image.Header().Get("Content-Type"))
	}
	if _, workspace, ok := strings.Cut(w.Body.String(), `<main class="workspace">`); !ok || strings.Contains(workspace, `/lifecycle`) {
		t.Error("instance controls belong in the sidebar, not the conversation")
	}
	if strings.Contains(w.Body.String(), `<img src=x>`) {
		t.Error("raw HTML leaked into the conversation")
	}
	fragment := request(http.MethodGet, "/worker/worker/history?session=session&before=previous", true, "", nil).Body.String()
	previous := -1
	for _, want := range []string{"Older prompt", "Checking files", "printf example", "command output", "Finished checking"} {
		index := strings.Index(fragment, want)
		if index <= previous {
			t.Fatalf("transcript missing or out of order at %q: %s", want, fragment)
		}
		previous = index
	}
	_, execution, _ := strings.Cut(fragment, `data-entry="tool:one"`)
	header, _, _ := strings.Cut(execution, "</summary>")
	if !strings.Contains(header, "<strong>Checking files</strong>") || !strings.Contains(header, ">Done</span>") || strings.Contains(header, "printf example") {
		t.Errorf("expected narration and status, not raw command, in collapsed header: %s", header)
	}
	if !strings.Contains(execution, "<pre><code>printf example</code></pre>") || strings.Contains(fragment, `&#34;command&#34;`) || strings.Contains(w.Body.String(), "Execution details") {
		t.Error("execution should have its own row and readable command on expand")
	}
	if strings.Contains(fragment, `<p>Checking files</p>`) && strings.Index(fragment, `<p>Checking files</p>`) < strings.Index(fragment, `data-entry="tool:one"`) {
		t.Error("narration used in a header should not also render as a separate chat message")
	}
	editPath := "/worker/worker/queue/edit"
	edit := url.Values{"session": {"session"}, "id": {"queued-1"}, "text": {"Revised follow-up"}}
	w = request(http.MethodPost, editPath, true, "http://example.com", edit)
	if w.Header().Get("HX-Trigger") != "queueEditSaved" {
		t.Fatalf("queue edit not acknowledged: %s", w.Body.String())
	}
	if raw := string(<-commands); !strings.Contains(raw, `"type":"edit_queued_text"`) || !strings.Contains(raw, `"expected_session_id":"session"`) || !strings.Contains(raw, `"id":"queued-1"`) || !strings.Contains(raw, `"text":"Revised follow-up"`) {
		t.Fatalf("unexpected queue edit: %s", raw)
	}
	edit.Set("id", "gone")
	w = request(http.MethodPost, editPath, true, "http://example.com", edit)
	if w.Header().Get("HX-Trigger") != "" || !strings.Contains(w.Body.String(), "Your edit was not sent") {
		t.Fatalf("missing stale-edit error: %s", w.Body.String())
	}
	setQueue := func(paused string, extra ...string) {
		items := append([]string{`{"id":"steer-1","kind":"steer","editable":true,"text":"Steer text"}`, `{"id":"queued-1","kind":"follow_up","editable":true,"text":"A pending follow-up"}`}, extra...)
		queueJSON.Store(new(fmt.Sprintf(`{"steering":1,"follow_up":%d,%s"items":[%s]}`, 1+len(extra), paused, strings.Join(items, ","))))
	}
	expectQueueCommands := func(want ...string) {
		t.Helper()
		for _, expected := range want {
			select {
			case raw := <-commands:
				var cmd struct {
					Type string
					Args struct{ Queue, ID string }
				}
				if err := json.Unmarshal(raw, &cmd); err != nil {
					t.Fatal(err)
				}
				if got := cmd.Type + ":" + cmd.Args.Queue + cmd.Args.ID; got != expected {
					t.Fatalf("queue command = %s, want %s", got, expected)
				}
			default:
				t.Fatalf("missing queue command %s", expected)
			}
		}
		if len(commands) != 0 {
			t.Fatalf("unexpected extra command: %s", <-commands)
		}
	}
	queuePost := func(action string, form url.Values) *httptest.ResponseRecorder {
		form.Set("session", "session")
		return request(http.MethodPost, "/worker/worker/queue/"+action, true, "http://example.com", form)
	}
	const bothPaused = `"steering_paused":true,"follow_up_paused":true,`

	// Edit all pauses both queues and opens every message.
	setQueue("")
	w = queuePost("pause", url.Values{})
	expectQueueCommands("pause_queue:steer", "pause_queue:follow_up")
	for _, want := range []string{`name="resume" value="steer"`, `name="resume" value="follow_up"`, "Steer text", "A pending follow-up"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("edit-all dialog missing %q: %s", want, w.Body.String())
		}
	}
	if w.Header().Get("HX-Trigger-After-Settle") != "queueEditOpen" {
		t.Error("pause should open the editor")
	}

	// Saving sends only changed text, then resumes steering first.
	setQueue(bothPaused)
	w = queuePost("edit", url.Values{"id": {"steer-1", "queued-1"}, "text": {"Steer text", "Revised"}, "resume": {"follow_up", "steer"}})
	expectQueueCommands("edit_queued_text:queued-1", "resume_queue:steer", "resume_queue:follow_up")
	if w.Header().Get("HX-Trigger") != "queueEditSaved" {
		t.Fatalf("edit all not acknowledged: %s", w.Body.String())
	}

	// A pause set elsewhere isn't this dialog's to release.
	setQueue(`"steering_paused":true,`)
	w = queuePost("pause", url.Values{"id": {"queued-1"}})
	expectQueueCommands("pause_queue:follow_up")
	if body := w.Body.String(); !strings.Contains(body, `name="resume" value="follow_up"`) || strings.Contains(body, `name="resume" value="steer"`) || strings.Contains(body, "Steer text") {
		t.Errorf("single edit dialog: %s", body)
	}

	// A partial failure keeps unsaved text, drops saved rows, and stays paused.
	setQueue(bothPaused, `{"id":"gone","kind":"follow_up","editable":true,"text":"Original"}`)
	w = queuePost("edit", url.Values{"id": {"queued-1", "gone"}, "text": {"Saved", "Keep this"}, "resume": {"follow_up"}})
	expectQueueCommands("edit_queued_text:queued-1")
	if body := w.Body.String(); w.Header().Get("HX-Trigger") != "" || !strings.Contains(body, "Saved 1.") || !strings.Contains(body, "Keep this") || !strings.Contains(body, "readonly") || strings.Contains(body, `value="queued-1"`) {
		t.Errorf("partial failure: %s", body)
	}

	// Resume with no named queue releases whatever is paused.
	setQueue(`"steering_paused":true,`)
	queuePost("resume", url.Values{})
	expectQueueCommands("resume_queue:steer")

	// An older worker can still edit, with a warning that it can't pause.
	setQueue("", `{"id":"old-worker","kind":"follow_up","editable":true,"text":"Old"}`)
	w = queuePost("pause", url.Values{})
	expectQueueCommands()
	if !strings.Contains(w.Body.String(), "can't pause its queue") || strings.Contains(w.Body.String(), `name="resume"`) {
		t.Errorf("older worker dialog: %s", w.Body.String())
	}

	// A paused, empty queue must stay visible or nothing can resume it.
	queueJSON.Store(new(`{"steering":0,"follow_up":0,"follow_up_paused":true,"items":[]}`))
	if page := request(http.MethodGet, "/worker/worker", true, "", nil).Body.String(); !strings.Contains(page, "data-queue-resume") || strings.Contains(page, `class="queue-panel mutation" hidden`) {
		t.Error("paused empty queue has no visible Resume")
	}
	queueJSON.Store(new(defaultQueue))
	for _, check := range []struct{ path, want, absent string }{
		{"/worker/worker", `id="session-chip"`, ""},
		{"/worker/worker/feed?session=session", `hx-swap-oob="true"`, ""},
		{"/worker/worker/controls?tab=session&session=session", "$1.37", ""},
		{"/worker/worker/controls?tab=session&session=session", `value="Controls work"`, ""},
		{"/worker/worker/controls?tab=session&session=session", `value="all" selected`, ""},
		{"/worker/worker/controls?tab=model&session=session", `aria-checked="true">medium`, "xhigh"},
		{"/worker/worker/controls?tab=model&session=session", `id="model-filters" hx-preserve`, ""},
		{"/worker/worker/controls?tab=model&session=session", `aria-pressed="true" data-scope="scoped"`, ""},
		{"/worker/worker/controls?tab=sessions&session=session", "Older work", ""},
		{"/worker/worker/controls?tab=tools&session=session", "Sign-in needed", ""},
		{"/worker/worker/controls?tab=tools&session=session", "can't report plugins yet", ""},
		{"/worker/worker/controls?tab=model&session=old", "switched sessions", ""},
		{"/worker/worker/controls/pick?kind=fork&session=session", "First question", ""},
		{"/worker/worker/commands", "skill:review", `"help"`},
		{"/worker/worker/last-reply", "Last reply", ""},
	} {
		w := request(http.MethodGet, check.path, true, "", nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, check.want) || (check.absent != "" && strings.Contains(body, check.absent)) {
			t.Fatalf("GET %s: %d, want %q without %q: %.2000s", check.path, w.Code, check.want, check.absent, body)
		}
	}
	// Only the cursor's line goes to Tau, and cursors count UTF-16 units: "é" is
	// one unit but two bytes, and "😀" is two units but four bytes.
	w = request(http.MethodPost, "/worker/worker/complete", true, "http://example.com", url.Values{"text": {"first\né😀 @ma\nlast"}, "cursor": {"13"}})
	var choices []pathChoice
	if err := json.Unmarshal(w.Body.Bytes(), &choices); w.Code != 200 || err != nil {
		t.Fatalf("complete: %d %v %s", w.Code, err, w.Body.String())
	}
	want := []pathChoice{
		{Label: "main.go", Text: "first\né😀 @main.go \nlast", Cursor: 19},
		{Label: "cmd/", Description: "folder", Text: "first\né😀 @cmd/\nlast", Cursor: 15},
	}
	if !reflect.DeepEqual(choices, want) {
		t.Fatalf("complete: got %+v, want %+v", choices, want)
	}
	controlsPath := "/worker/worker/controls"
	checkModels := func(w *httptest.ResponseRecorder, options ...string) {
		t.Helper()
		body := w.Body.String()
		for _, option := range options {
			_, rest, ok := strings.Cut(body, `value="`+option+`" role="radio"`)
			if !ok {
				t.Fatalf("missing or out-of-order model %s: %s", option, w.Body.String())
			}
			body = rest
		}
	}
	modelTab := controlsPath + "?tab=model&session=session"
	w = request(http.MethodGet, modelTab, true, "", nil)
	checkModels(w, "anthropic/claude-opus-5-5", "anthropic/claude-haiku-4-5", "openai-codex/gpt-6-luna")
	if !strings.Contains(w.Body.String(), `name="pinned_model" value="anthropic/claude-opus-5-5"`) {
		t.Fatal("model forms must carry the pinned model to preserve the order")
	}
	w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"session"}, "tab": {"model"}, "action": {"model"}, "value": {"openai-codex/gpt-6-luna"}, "pinned_model": {"anthropic/claude-opus-5-5"}})
	if w.Header().Get("HX-Trigger") != "conversationRefresh" || !strings.Contains(w.Body.String(), `value="openai-codex/gpt-6-luna" role="radio" aria-checked="true"`) {
		t.Fatalf("model change: %s", w.Body.String())
	}
	checkModels(w, "anthropic/claude-opus-5-5", "anthropic/claude-haiku-4-5", "openai-codex/gpt-6-luna")
	checkModels(request(http.MethodGet, modelTab, true, "", nil), "openai-codex/gpt-6-luna", "anthropic/claude-haiku-4-5", "anthropic/claude-opus-5-5")
	if raw := string(<-commands); !strings.Contains(raw, `"type":"set_model"`) || !strings.Contains(raw, `"model_spec":"openai-codex/gpt-6-luna"`) {
		t.Fatalf("unexpected model command: %s", raw)
	}
	w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"session"}, "tab": {"session"}, "action": {"new"}})
	if w.Header().Get("HX-Redirect") != "/worker/worker" {
		t.Fatalf("new session should reload the conversation: %v %s", w.Header(), w.Body.String())
	}
	if raw := string(<-commands); !strings.Contains(raw, `"type":"new_session"`) {
		t.Fatalf("unexpected new-session command: %s", raw)
	}
	w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"old"}, "tab": {"session"}, "action": {"new"}})
	if w.Header().Get("HX-Redirect") != "" || !strings.Contains(w.Body.String(), "switched sessions") || len(commands) != 0 {
		t.Fatalf("stale session must not start a new one: %s", w.Body.String())
	}
	// A composer command answers in the notice line, with or without a body.
	for _, body := range []string{"", "keep the API notes"} {
		w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"session"}, "action": {"compact"}, "value": {body}, "inline": {"1"}})
		if got := w.Body.String(); w.Header().Get("HX-Trigger") != "conversationRefresh" || !strings.Contains(got, `class="success">History compacted.`) || strings.Contains(got, "data-panel") {
			t.Fatalf("inline compact: %s", got)
		}
		if raw := string(<-commands); !strings.Contains(raw, `"type":"compact"`) || !strings.Contains(raw, body) {
			t.Fatalf("unexpected compact command: %s", raw)
		}
	}
	w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"session"}, "action": {"compact"}, "value": {"too small"}, "inline": {"1"}})
	if got := w.Body.String(); !strings.Contains(got, `class="error">nothing to compact`) || strings.Contains(got, "data-panel") {
		t.Fatalf("inline compact failure: %s", got)
	}
	// A failure in the panel keeps the panel's contents under the error.
	w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"session"}, "tab": {"session"}, "action": {"compact"}, "value": {"too small"}})
	if got := w.Body.String(); !strings.Contains(got, "nothing to compact") || !strings.Contains(got, "stat-grid") || strings.Contains(got, "History compacted") {
		t.Fatalf("panel compact failure: %s", got)
	}
	w = request(http.MethodGet, controlsPath+"?tab=sessions&session=session", true, "", nil)
	if got := w.Body.String(); !strings.Contains(got, `value="resume"`) || strings.Contains(got, "Switch this instance") {
		t.Fatalf("resume should run without a confirmation: %s", got)
	}
	path := "/worker/worker/prompt"
	long := strings.Repeat("a long prompt ", 4000) // Well past any limit Tau UI used to invent.
	f := url.Values{"session": {"session"}, "text": {long}, "behavior": {"follow_up"}}
	if w := request(http.MethodPost, path, false, "http://example.com", f); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request: %d", w.Code)
	}
	if w := request(http.MethodPost, path, true, "https://example.org", f); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin request: %d", w.Code)
	}
	w = request(http.MethodPost, path, true, "http://example.com", f)
	if w.Code != http.StatusOK || w.Header().Get("HX-Trigger") != "promptAccepted" || w.Body.Len() != 0 {
		t.Fatalf("expected a silent acknowledgement: %d %s", w.Code, w.Body.String())
	}
	w = request(http.MethodPost, path, true, "http://example.com", url.Values{"session": {"session"}})
	if w.Header().Get("HX-Trigger") != "" || !strings.Contains(w.Body.String(), "Enter a message or attach a file.") {
		t.Fatalf("expected a visible error: %d %s", w.Code, w.Body.String())
	}
	select {
	case raw := <-commands:
		var command struct {
			Type string
			Args map[string]any
		}
		if err := json.Unmarshal(raw, &command); err != nil || command.Type != "send_message" ||
			command.Args["expected_session_id"] != "session" || command.Args["text"] != long || command.Args["streaming_behavior"] != "follow_up" ||
			command.Args["expand_refs"] != true {
			t.Fatalf("unexpected command %s: %v", raw, err)
		}
	default:
		t.Fatal("no prompt reached Tau")
	}
	if len(commands) != 0 {
		t.Fatal("prompt sent more than once")
	}
	f.Set("text", strings.Repeat("x", 1<<20))
	w = request(http.MethodPost, path, true, "http://example.com", f)
	if w.Header().Get("HX-Trigger") != "" || !strings.Contains(w.Body.String(), "1 MiB message limit") || len(commands) != 0 {
		t.Fatalf("expected Tau's frame limit to be explained: %s", w.Body.String())
	}
	var body bytes.Buffer
	upload := multipart.NewWriter(&body)
	if err := upload.WriteField("session", "session"); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), 2<<20) // Larger than one RPC frame.
	for i := range 6 {
		file, err := upload.CreateFormFile("attachment", fmt.Sprintf("notes-%d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(big); err != nil {
			t.Fatal(err)
		}
	}
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, &body)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Content-Type", upload.FormDataContentType())
	r.SetBasicAuth("tau", "test-password")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("HX-Trigger") != "promptAccepted" || len(commands) != 7 {
		t.Fatalf("attachments not sent: %d commands, %s", len(commands), w.Body.String())
	}
	for i := range 6 {
		if raw := string(<-commands); raw != fmt.Sprintf("attached notes-%d.txt: %d bytes", i, len(big)) {
			t.Errorf("wrong attachment: %s", raw)
		}
	}
	if raw := string(<-commands); !strings.Contains(raw, `"attachment_ids":["file-id","file-id","file-id","file-id","file-id","file-id"]`) {
		t.Errorf("attachment missing in prompt RPC: %s", raw)
	}
	for _, op := range []string{"stop", "restart"} {
		w = request(http.MethodPost, "/worker/worker/lifecycle", true, "http://example.com", url.Values{"op": {op}, "session": {"old"}})
		if !strings.Contains(w.Body.String(), "switched sessions") || w.Header().Get("HX-Redirect") != "" {
			t.Fatalf("%s accepted stale session: %s", op, w.Body.String())
		}
		w = request(http.MethodPost, "/worker/worker/lifecycle", true, "http://example.com", url.Values{"op": {op}, "session": {"session"}, "active": {"another-worker"}})
		if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "" || w.Header().Get("HX-Trigger-After-Settle") != "instanceListRefresh" {
			t.Fatalf("%s should refresh the list without leaving the current conversation: %s", op, w.Body.String())
		}
		w = request(http.MethodPost, "/worker/worker/lifecycle", true, "http://example.com", url.Values{"op": {op}, "session": {"session"}, "active": {"worker"}})
		want := "/"
		if op == "restart" {
			want = "/worker/restarted"
		}
		if w.Header().Get("HX-Redirect") != want {
			t.Fatalf("%s: %s", op, w.Body.String())
		}
	}
}

// testPNG is a 1x1 PNG.
var testPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

var testLimited = coord.Instance{
	ID: "limited", CWD: "/test/Limited project", SessionID: "limited-session", State: coord.StateUsageLimited,
	StateDetail: coord.StateDetail{Provider: "codex", ResetsAt: time.Unix(1000, 0)}, LastActivity: time.Unix(10, 0),
}

func fakeRPC(t *testing.T, link *coord.Link, commands chan<- json.RawMessage, modelJSON, queueJSON *atomic.Pointer[string]) {
	t.Helper()
	defer func() {
		if err := link.Close(); err != nil {
			t.Error(err)
		}
	}()
	hello, err := link.Read()
	if err != nil {
		t.Error(err)
		return
	}
	if err := link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, ClientID: hello.ClientID, Operations: []string{coord.OperationInstanceRestart, coord.OperationSavedSessionLaunch}}); err != nil {
		t.Error(err)
		return
	}
	inst := coord.Instance{ID: "worker", CWD: "/home/test/project", SessionID: "session", PendingInputs: 1}
	stopped := false
	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		reply := coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID}
		switch f.Type {
		case coord.TypeListInstances, coord.TypeWatchInstances:
			if !stopped {
				reply.Instances = []coord.Instance{
					inst,
					testLimited,
					{ID: "busy", State: coord.StateWorking, LastActivity: time.Unix(40, 0)},
					{ID: "exited", LinkState: coord.LinkExited, CWD: "/test/Exited project", LastActivity: time.Unix(50, 0)},
					{ID: "ready-old", State: coord.StateAwaitingUser, LastActivity: time.Unix(20, 0)},
					{ID: "ready-new", State: coord.StateAwaitingUser, LastActivity: time.Unix(30, 0)},
				}
			}
		case coord.TypeStopInstance:
			if f.InstanceID != inst.ID || (f.SessionID != "" && f.SessionID != inst.SessionID) {
				t.Errorf("wrong lifecycle target: %+v", f)
			}
			stopped = true
			reply.Result = json.RawMessage(`{"type":"response","ok":true}`)
		case coord.TypeStartInstance:
			if f.SessionID == "" {
				cwd, err := json.Marshal(f.CWD)
				if err != nil {
					t.Error(err)
					return
				}
				commands <- cwd
				reply.Instance = &coord.Instance{ID: "started", CWD: f.CWD}
				break
			}
			if !stopped || f.SessionID != inst.SessionID || f.CWD != inst.CWD {
				t.Errorf("wrong restart target: %+v", f)
			}
			reply.Instance = &coord.Instance{ID: "restarted", CWD: inst.CWD, SessionID: inst.SessionID}
		case coord.TypeCommand:
			var cmd struct {
				Type string
				Args map[string]json.RawMessage
			}
			if err := json.Unmarshal(f.Command, &cmd); err != nil {
				t.Error(err)
				return
			}
			switch cmd.Type {
			case "get_snapshot":
				reply.Result = json.RawMessage(`{"position":{"instance_id":"worker","epoch":"test","offset":4},"instance":{"instance_id":"worker","cwd":"/home/test/project","session_id":"session","pending_inputs":1},"state":{"session_id":"session","model":"anthropic/claude-opus-5-5","thinking_level":"medium","stats":{"model":"anthropic/claude-opus-5-5","thinking_level":"medium","context_used_pct":42,"context_window":200000,"total_tokens":84000},"is_streaming":true,"queue":` + *queueJSON.Load() + `},"history":{"before":"previous","entries":[{"entry_id":"photo","message":{"Role":"user","Content":[{"type":"text","text":"See this mockup"},{"type":"text","text":"<file name=\"mockup.png\">[image content]</file>"},{"type":"image","mimeType":"image/png"}]}},{"entry_id":"current","message":{"Role":"assistant","Content":[{"type":"text","text":"**Bold**\n\n<img src=x>\n\n` + "```" + `sh\necho \"hello\"\n` + "```" + `"}]}}]},"live":{"messages":[{"id":"reply","role":"assistant","text":"Partial reply","truncated":true}],"tools":[{"id":"live-read","name":"read","input":"{\"path\":\"notes.txt\"}","progress":"Reading lines"}]},"ui_requests":[{"id":"id","method":"confirm","title":"Approve changes"}]}`)
			case "get_history":
				reply.Result = json.RawMessage(`{"entries":[{"entry_id":"previous","message":{"Role":"user","Text":"Older prompt"}},{"entry_id":"call","message":{"Role":"assistant","Text":"Checking filesFinished checking","Content":[{"type":"text","text":"Checking files"},{"type":"toolCall","id":"one","name":"bash","arguments":{"command":"printf example"}},{"type":"text","text":"Finished checking"}]}},{"entry_id":"res","message":{"Role":"toolResult","ToolCallID":"one","Text":"command output"}}]}`)
			case "get_entry_content":
				var data []byte
				switch {
				case string(cmd.Args["image_index"]) == "0" && string(cmd.Args["entry_id"]) == `"photo"`:
					data = []byte(base64.StdEncoding.EncodeToString(testPNG))
				case string(cmd.Args["live_kind"]) == `"message"` && string(cmd.Args["entry_id"]) == `"reply"`:
					data = []byte(`{"id":"reply","text":"Start of reply. Partial reply"}`)
				default:
					t.Errorf("unexpected content request: %s", f.Command)
				}
				result, err := json.Marshal(map[string]any{"type": "response", "ok": true, "result": map[string]any{"data": data, "more": false}})
				if err != nil {
					t.Error(err)
					return
				}
				reply.Result = result
			case "attach_file":
				var path string
				if err := json.Unmarshal(cmd.Args["path"], &path); err != nil {
					t.Error(err)
					return
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Error(err)
					return
				}
				commands <- json.RawMessage(fmt.Sprintf("attached %s: %d bytes", filepath.Base(path), len(data)))
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"attachment":{"id":"file-id"}}}`)
			case "edit_queued_text":
				if string(cmd.Args["id"]) == `"gone"` {
					reply.Result = json.RawMessage(`{"type":"response","ok":false,"error":{"code":"queue_changed","message":"Your edit was not sent."}}`)
				} else {
					commands <- f.Command
					reply.Result = json.RawMessage(`{"type":"response","ok":true}`)
				}
			case "pause_queue", "resume_queue":
				if cmd.Type == "pause_queue" && strings.Contains(*queueJSON.Load(), "old-worker") {
					reply.Result = json.RawMessage(`{"type":"response","ok":false,"error":{"code":"unknown_command","message":"unknown command: pause_queue"}}`)
					break
				}
				commands <- f.Command
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"ok":true}}`)
			case "get_state":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"session_id":"session","model":` + *modelJSON.Load() + `,"thinking_level":"medium","auto_retry":true,"steering_mode":"one-at-a-time","follow_up_mode":"all","session_name":"Controls work"}}`)
			case "get_session_stats":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"message_count":48,"total_tokens":84000,"context_window":200000,"context_used_pct":42,"auto_compaction":true,"usage":{"cost":{"input":1.0,"output":0.37}}}}`)
			case "get_available_models":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"models":[{"id":"claude-opus-5-5","provider":"anthropic","name":"Claude Opus 5.5","context_window":200000,"reasoning":true,"thinking_levels":["off","low","medium","high"]},{"id":"claude-haiku-4-5","provider":"anthropic"},{"id":"gpt-6-luna","provider":"openai-codex","context_window":400000,"thinking_levels":["off"]}]}}`)
			case "get_scoped_models":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"patterns":["anthropic/*"],"models":[{"model":"anthropic/claude-opus-5-5"},{"model":"anthropic/claude-haiku-4-5"}]}}`)
			case "list_sessions":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"sessions":[{"id":"older","name":"Older work","last_modified":"2026-11-20T10:00:00Z","message_count":12},{"id":"session","name":"Controls work","last_modified":"2026-11-22T10:00:00Z","message_count":48,"pinned":true}]}}`)
			case "get_mcp_servers":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"servers":[{"name":"github","status":"connected","tool_count":12},{"name":"linear","status":"needs-auth"}]}}`)
			case "get_plugins":
				reply.Result = json.RawMessage(`{"type":"response","ok":false,"error":{"code":"unknown_command","message":"unknown command: get_plugins"}}`)
			case "get_fork_entries", "get_forget_entries":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"entries":[{"entry_id":"e1","role":"user","text":"First question"}]}}`)
			case "get_commands":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"commands":[{"name":"help","source":"builtin"},{"name":"skill:review","description":"Review code","source":"skill"}]}}`)
			case "complete_path":
				if string(cmd.Args["text"]) != `"é😀 @ma"` || string(cmd.Args["cursor"]) != "10" {
					t.Errorf("complete_path got %s", f.Command)
				}
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"prefix":"@ma","items":[{"value":"@main.go","label":"main.go"},{"value":"@cmd/","label":"cmd/","description":"folder"}]}}`)
			case "get_last_assistant_text":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"text":"Last reply"}}`)
			case "compact":
				if strings.Contains(string(cmd.Args["custom_instructions"]), "too small") {
					reply.Result = json.RawMessage(`{"type":"response","ok":false,"error":{"code":"command_failed","message":"nothing to compact (session too small)"}}`)
					break
				}
				commands <- f.Command
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"ok":true}}`)
			case "set_model", "set_thinking_level", "new_session":
				if cmd.Type == "set_model" {
					modelJSON.Store(new(string(cmd.Args["model_spec"])))
				}
				commands <- f.Command
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"ok":true}}`)
			case "send_message":
				commands <- f.Command
				reply.Result = json.RawMessage(`{"type":"response","ok":true}`)
			default:
				t.Errorf("unexpected command: %s", f.Command)
				return
			}
		case coord.TypeAttach, coord.TypeDetach: // The restart watcher follows open pages.
		default:
			t.Errorf("unexpected frame: %s", f.Type)
			return
		}
		if err := link.Send(reply); err != nil {
			t.Error(err)
			return
		}
	}
}

func TestOutdatedInstanceCard(t *testing.T) {
	const (
		button  = `class="instance-update"`
		warning = `class="instance-outdated"`
		confirm = `may have a run in progress`
	)
	current := coord.Instance{ID: "worker", CWD: "/test/project", SessionID: "session", State: coord.StateAwaitingUser}
	outdated := current
	outdated.Outdated = true
	with := func(change func(*coord.Instance)) coord.Instance {
		i := outdated
		change(&i)
		return i
	}
	for _, test := range []struct {
		name                     string
		item                     coord.Instance
		noRestart                bool
		hasButton, warns, prompt bool
	}{
		{name: "current build", item: current},
		{name: "idle restarts without asking", item: outdated, hasButton: true},
		{name: "working asks first", item: with(func(i *coord.Instance) { i.State = coord.StateWorking }), hasButton: true, prompt: true},
		{name: "usage limit asks first", item: with(func(i *coord.Instance) { i.State = coord.StateUsageLimited }), hasButton: true, prompt: true},
		{name: "error restarts without asking", item: with(func(i *coord.Instance) { i.State = coord.StateError }), hasButton: true},
		{name: "unknown asks first", item: with(func(i *coord.Instance) { i.State = "busy" }), hasButton: true, prompt: true},
		{name: "awaiting input asks first", item: with(func(i *coord.Instance) { i.PendingInputs = 1 }), hasButton: true, prompt: true},
		{name: "daemon can't restart", item: outdated, noRestart: true, warns: true},
		{name: "no session to resume", item: with(func(i *coord.Instance) { i.SessionID = "" }), warns: true},
		{name: "disconnected", item: with(func(i *coord.Instance) { i.LinkState = "disconnected" }), warns: true},
		{name: "exited", item: with(func(i *coord.Instance) { i.LinkState = coord.LinkExited })},
	} {
		t.Run(test.name, func(t *testing.T) {
			card := summary(test.item)
			card.Restart = !test.noRestart
			var out strings.Builder
			if err := pages.ExecuteTemplate(&out, "instance-card", card); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if strings.Contains(got, button) != test.hasButton {
				t.Errorf("update button = %v, want %v: %s", !test.hasButton, test.hasButton, got)
			}
			if strings.Contains(got, warning) != test.warns {
				t.Errorf("passive warning = %v, want %v: %s", !test.warns, test.warns, got)
			}
			if strings.Contains(got, confirm) != test.prompt {
				t.Errorf("confirmation = %v, want %v: %s", !test.prompt, test.prompt, got)
			}
		})
	}
}

func TestUsageLimitFeed(t *testing.T) {
	var s client.Snapshot
	if err := json.Unmarshal([]byte(`{
		"history":{"entries":[
			{"entry_id":"failed","message":{"Role":"assistant","IsError":true,"ErrorMessage":"openai codex: status 429: <slow down>","Content":[{"type":"text"}]}},
			{"entry_id":"partial","message":{"Role":"assistant","ErrorMessage":"stream cut","Content":[{"type":"text","text":"Half an answer"}]}}
		]},
		"live":{"messages":[],"tools":[],"bash":[],"usage_limit_wait":{"provider":"openai","resets_at":"2026-11-23T18:30:00Z"}}
	}`), &s); err != nil {
		t.Fatal(err)
	}
	var live liveActivity
	if err := json.Unmarshal(s.Live, &live); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	render(rec, "feed", feedView{
		Session: "session", Busy: true, UsageWait: live.UsageLimitWait,
		Messages: contentSource{"worker", "session"}.transcript(s.History.Entries, live),
	})
	body := rec.Body.String()
	for _, want := range []string{`data-entry="entry:failed:error"`, `openai codex: status 429: &lt;slow down&gt;`, `data-resets-at="2026-11-23T18:30:00Z"`, `openai usage limit`, `Half an answer`} {
		if !strings.Contains(body, want) {
			t.Errorf("feed is missing %q:\n%s", want, body)
		}
	}
	for _, reject := range []string{`Tau is working`, `stream cut`} {
		if strings.Contains(body, reject) {
			t.Errorf("feed should not contain %q:\n%s", reject, body)
		}
	}
}

func TestLinkify(t *testing.T) {
	got := string(linkify(`Sign in to "linear" at https://auth.example/x?a=1&b=<2>, then paste.`))
	want := `Sign in to &#34;linear&#34; at <a href="https://auth.example/x?a=1&amp;b=" target="_blank" rel="noopener noreferrer">https://auth.example/x?a=1&amp;b=</a>&lt;2&gt;, then paste.`
	if got != want {
		t.Fatalf("linkify:\n got %s\nwant %s", got, want)
	}
}

// The Interrupt button sits inside the multipart prompt form, but its handler
// only reads urlencoded bodies, so it must opt out of the inherited encoding.
func TestInterruptSendsURLEncodedSession(t *testing.T) {
	page, err := os.ReadFile("web/ui.html")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(page), `hx-post="/worker/{{.ID}}/interrupt"`)
	if !ok {
		t.Fatal("no Interrupt button")
	}
	tag, _, _ := strings.Cut(rest, ">")
	for _, want := range []string{`hx-encoding="unset"`, `hx-params="session"`, `hx-include="#prompt-form [name=session]"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("Interrupt button missing %s: %s", want, tag)
		}
	}
}
