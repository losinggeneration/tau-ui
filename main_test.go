package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/rpc/coord"
)

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
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				return
			}
			fakeRPC(t, coord.NewLink(conn), commands)
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		<-done
	})
	h := newApp(socket, "test-password")
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
		if w.Code != 200 || !strings.Contains(w.Body.String(), check.want) {
			t.Fatalf("GET %s: %d, missing %q: %.1500s", check.path, w.Code, check.want, w.Body.String())
		}
	}
	if list := request(http.MethodGet, "/instances", true, "", nil).Body.String(); strings.Contains(list, `hx-confirm="Restart`) || !strings.Contains(list, `data-busy-label="Restarting…"`) {
		t.Error("restart should run without a confirmation dialog and show progress on the card")
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
	for _, check := range []struct{ path, want, absent string }{
		{"/worker/worker", `id="session-chip"`, ""},
		{"/worker/worker/feed?session=session", `hx-swap-oob="true"`, ""},
		{"/worker/worker/controls?tab=session&session=session", "$1.37", ""},
		{"/worker/worker/controls?tab=session&session=session", `value="Controls work"`, ""},
		{"/worker/worker/controls?tab=session&session=session", `value="all" selected`, ""},
		{"/worker/worker/controls?tab=model&session=session", `aria-checked="true">medium`, "xhigh"},
		{"/worker/worker/controls?tab=model&session=session", "data-scoped-only", ""},
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
	controlsPath := "/worker/worker/controls"
	w = request(http.MethodPost, controlsPath, true, "http://example.com", url.Values{"session": {"session"}, "tab": {"model"}, "action": {"model"}, "value": {"openai-codex/gpt-6-luna"}})
	if w.Header().Get("HX-Trigger") != "conversationRefresh" || !strings.Contains(w.Body.String(), `data-panel="model"`) {
		t.Fatalf("model change: %s", w.Body.String())
	}
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
			Args map[string]string
		}
		if err := json.Unmarshal(raw, &command); err != nil || command.Type != "send_message" ||
			command.Args["expected_session_id"] != "session" || command.Args["text"] != long || command.Args["streaming_behavior"] != "follow_up" {
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

func fakeRPC(t *testing.T, link *coord.Link, commands chan<- json.RawMessage) {
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
		case coord.TypeListInstances:
			if !stopped {
				reply.Instances = []coord.Instance{inst}
			}
		case coord.TypeStopInstance:
			if f.InstanceID != inst.ID || (f.SessionID != "" && f.SessionID != inst.SessionID) {
				t.Errorf("wrong lifecycle target: %+v", f)
			}
			stopped = true
			reply.Result = json.RawMessage(`{"type":"response","ok":true}`)
		case coord.TypeStartInstance:
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
				reply.Result = json.RawMessage(`{"position":{"instance_id":"worker","epoch":"test","offset":4},"instance":{"instance_id":"worker","cwd":"/home/test/project","session_id":"session","pending_inputs":1},"state":{"session_id":"session","model":"anthropic/claude-opus-5-5","thinking_level":"medium","stats":{"model":"anthropic/claude-opus-5-5","thinking_level":"medium","context_used_pct":42,"context_window":200000,"total_tokens":84000},"is_streaming":true,"queue":{"steering":0,"follow_up":1,"items":[{"id":"queued-1","kind":"follow_up","editable":true,"text":"A pending follow-up"}]}},"history":{"before":"previous","entries":[{"entry_id":"photo","message":{"Role":"user","Content":[{"type":"text","text":"See this mockup"},{"type":"text","text":"<file name=\"mockup.png\">[image content]</file>"},{"type":"image","mimeType":"image/png"}]}},{"entry_id":"current","message":{"Role":"assistant","Content":[{"type":"text","text":"**Bold**\n\n<img src=x>\n\n` + "```" + `sh\necho \"hello\"\n` + "```" + `"}]}}]},"live":{"messages":[{"id":"reply","role":"assistant","text":"Partial reply","truncated":true}],"tools":[{"id":"live-read","name":"read","input":"{\"path\":\"notes.txt\"}","progress":"Reading lines"}]},"ui_requests":[{"id":"id","method":"confirm","title":"Approve changes"}]}`)
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
			case "get_state":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"session_id":"session","model":"anthropic/claude-opus-5-5","thinking_level":"medium","auto_retry":true,"steering_mode":"one-at-a-time","follow_up_mode":"all","session_name":"Controls work"}}`)
			case "get_session_stats":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"message_count":48,"total_tokens":84000,"context_window":200000,"context_used_pct":42,"auto_compaction":true,"usage":{"cost":{"input":1.0,"output":0.37}}}}`)
			case "get_available_models":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"models":[{"id":"claude-opus-5-5","provider":"anthropic","name":"Claude Opus 5.5","context_window":200000,"reasoning":true,"thinking_levels":["off","low","medium","high"]},{"id":"gpt-6-luna","provider":"openai-codex","context_window":400000,"thinking_levels":["off"]}]}}`)
			case "get_scoped_models":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"patterns":["anthropic/*"],"models":[{"model":"anthropic/claude-opus-5-5"}]}}`)
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
			case "get_last_assistant_text":
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"text":"Last reply"}}`)
			case "set_model", "set_thinking_level", "new_session":
				commands <- f.Command
				reply.Result = json.RawMessage(`{"type":"response","ok":true,"result":{"ok":true}}`)
			case "send_message":
				commands <- f.Command
				reply.Result = json.RawMessage(`{"type":"response","ok":true}`)
			default:
				t.Errorf("unexpected command: %s", f.Command)
				return
			}
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

func TestLinkify(t *testing.T) {
	got := string(linkify(`Sign in to "linear" at https://auth.example/x?a=1&b=<2>, then paste.`))
	want := `Sign in to &#34;linear&#34; at <a href="https://auth.example/x?a=1&amp;b=" target="_blank" rel="noopener noreferrer">https://auth.example/x?a=1&amp;b=</a>&lt;2&gt;, then paste.`
	if got != want {
		t.Fatalf("linkify:\n got %s\nwant %s", got, want)
	}
}
