package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
)

type app struct {
	socket string
}

var errDial = errors.New("Can't reach Tau. Check that the daemon is running.")

// ponytail: use short-lived RPC connections for polling; keep a live event
// subscription if polling snapshots becomes too expensive or too lossy.
func (a app) rpc(r *http.Request, timeout time.Duration, fn func(context.Context, *client.CoordinatedClient) error) error {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	dial, stopDial := context.WithTimeout(ctx, 2*time.Second)
	c, err := client.NewCoordinatedClient(dial, a.socket)
	stopDial()
	if err != nil {
		return errDial
	}
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("close Tau connection: %v", err)
		}
	}()
	return fn(ctx, c)
}

type instance struct {
	coord.Instance
	Label, Badge string
}

func summary(i coord.Instance) instance {
	out := instance{Instance: i, Label: "Ready", Badge: "ready"}
	switch {
	case i.LinkState == coord.LinkExited:
		out.Label, out.Badge = "Exited", "offline"
	case i.LinkState != "" && i.LinkState != coord.LinkConnected:
		out.Label, out.Badge = "Disconnected", "offline"
	case i.PendingInputs > 0:
		out.Label, out.Badge = "Needs input", "waiting"
	case i.State == coord.StateBusy:
		out.Label, out.Badge = "Running", "busy"
	}
	return out
}

type registry struct {
	Active, Error string
	Items         []instance
	Restart       bool
}

func loadRegistry(ctx context.Context, c *client.CoordinatedClient, active string) (registry, error) {
	v := registry{Active: active, Restart: restartSupported(c)}
	items, err := c.ListInstances(ctx)
	for _, i := range items {
		v.Items = append(v.Items, summary(i))
	}
	slices.SortStableFunc(v.Items, func(a, b instance) int { return b.LastActivity.Compare(a.LastActivity) })
	return v, err
}

func (a app) instances(w http.ResponseWriter, r *http.Request) {
	active := r.URL.Query().Get("active")
	v := registry{Active: active}
	err := a.rpc(r, 4*time.Second, func(ctx context.Context, c *client.CoordinatedClient) (err error) {
		v, err = loadRegistry(ctx, c, active)
		return err
	})
	if err != nil {
		v.Error = err.Error()
	}
	render(w, "instances", v)
}

type pageView struct {
	Registry         registry
	ID, Session, CWD string
	Feed             feedView
	Before           string
}

func (a app) page(w http.ResponseWriter, r *http.Request) {
	v := pageView{ID: r.PathValue("id")}
	err := a.rpc(r, 6*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		list, err := loadRegistry(ctx, c, v.ID)
		v.Registry = list
		if err != nil {
			return err
		}
		if v.ID == "" {
			return nil
		}
		for _, inst := range list.Items {
			if inst.ID == v.ID {
				v.CWD, v.Session = inst.CWD, inst.SessionID
				break
			}
		}
		f, err := snapshot(ctx, c, v.ID, "")
		if err != nil {
			v.Feed.Error = err.Error()
			return nil
		}
		v.Feed, v.Session, v.CWD, v.Before = f, f.Session, f.Instance.CWD, f.Before
		return nil
	})
	if err != nil {
		v.Registry.Error, v.Feed.Error = err.Error(), err.Error()
	}
	v.Feed.ID, v.Feed.Session = v.ID, v.Session
	render(w, "page", v)
}

type dialog struct {
	ID, Method, Title, Message, Placeholder string
	Options                                 []string
}

type queueView struct {
	Steering int                `json:"steering"`
	FollowUp int                `json:"follow_up"`
	Items    []sdk.QueuedPrompt `json:"items"`
}

func (q queueView) Total() int { return q.Steering + q.FollowUp }

type feedView struct {
	ID, Session, Before, Error string
	Instance                   instance
	Messages                   []message
	Requests                   []dialog
	Queue                      queueView
	Busy, Changed              bool
	// Header chip: model, thinking level, and context use.
	Model, Thinking string
	ContextPct      float64
	// OOB swaps the header chip in from a feed poll.
	OOB bool
}

// ModelShort drops the provider for the header chip.
func (v feedView) ModelShort() string {
	_, name, ok := strings.Cut(v.Model, "/")
	if !ok {
		return v.Model
	}
	return name
}

// ContextLevel colors the context meter.
func (v feedView) ContextLevel() string {
	switch {
	case v.ContextPct > 80:
		return "high"
	case v.ContextPct > 60:
		return "mid"
	default:
		return "low"
	}
}

func snapshot(ctx context.Context, c *client.CoordinatedClient, id, expected string) (feedView, error) {
	v := feedView{ID: id, Session: expected}
	s, err := c.SnapshotPreview(ctx, id)
	if err != nil {
		return v, err
	}
	v.Instance = summary(s.Instance)
	if expected != "" && expected != s.State.SessionID {
		v.Changed = true
		return v, nil
	}
	v.Session, v.Before = s.State.SessionID, s.History.Before
	v.Busy = s.State.IsStreaming
	if len(s.State.Model) > 0 {
		// Older workers may send the model as an object; the chip then uses stats.
		_ = json.Unmarshal(s.State.Model, &v.Model)
	}
	v.Thinking = s.State.ThinkingLevel
	if st := s.State.Stats; st != nil {
		v.Model, v.Thinking, v.ContextPct = cmp.Or(st.Model, v.Model), cmp.Or(st.ThinkingLevel, v.Thinking), st.ContextUsedPct
	}
	if len(s.State.Queue) > 0 {
		if err := json.Unmarshal(s.State.Queue, &v.Queue); err != nil {
			return v, fmt.Errorf("read Tau queue: %w", err)
		}
	}
	var live liveActivity
	if len(s.Live) != 0 {
		if err := json.Unmarshal(s.Live, &live); err != nil {
			return v, fmt.Errorf("read Tau live activity: %w", err)
		}
	}
	v.Messages = (contentSource{id, v.Session}).transcript(s.History.Entries, live)
	for _, raw := range s.UIRequests {
		var d dialog
		if err := json.Unmarshal(raw, &d); err != nil {
			return v, fmt.Errorf("read Tau input request: %w", err)
		}
		v.Requests = append(v.Requests, d)
	}
	return v, nil
}

func (a app) feed(w http.ResponseWriter, r *http.Request) {
	id, expected := r.PathValue("id"), r.URL.Query().Get("session")
	v := feedView{ID: id, Session: expected}
	err := a.rpc(r, 4*time.Second, func(ctx context.Context, c *client.CoordinatedClient) (err error) {
		v, err = snapshot(ctx, c, id, expected)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	v.OOB = true
	render(w, "feed", v)
}

type historyView struct {
	ID, Session, Before, Cursor, Error string
	Messages                           []message
}

func (a app) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := historyView{ID: r.PathValue("id"), Session: q.Get("session"), Cursor: q.Get("before")}
	if v.Cursor == "" || v.Session == "" {
		http.Error(w, "Missing history position", http.StatusBadRequest)
		return
	}
	err := a.rpc(r, 5*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		page, err := c.HistoryPreview(ctx, v.ID, v.Session, v.Cursor)
		if err != nil {
			return err
		}
		v.Messages, v.Before = (contentSource{v.ID, v.Session}).transcript(page.Entries, liveActivity{}), page.Before
		return nil
	})
	if err != nil {
		v.Error, v.Before = err.Error(), v.Cursor
	}
	render(w, "history", v)
}

func form(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

func parsePromptForm(w http.ResponseWriter, r *http.Request) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("invalid prompt form: %w", err)
	}
	switch media {
	case "multipart/form-data":
		// Files beyond the memory threshold are spooled to disk.
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return fmt.Errorf("Could not read the message and attachments: %w", err)
		}
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return fmt.Errorf("Could not read the message: %w", err)
		}
	default:
		return errors.New("Unsupported prompt format.")
	}
	return nil
}

type notice struct {
	Text  string
	Error bool
}

func result(w http.ResponseWriter, good string, err error) {
	v := notice{Text: good}
	if err != nil {
		v.Error = true
		v.Text = err.Error()
		if errors.Is(err, client.ErrOutcomeUnknown) {
			v.Text = "Tau may have received this. Check the conversation or instance list before trying again. Nothing was resent."
		}
	}
	render(w, "notice", v)
}

func (a app) command(r *http.Request, name string, args any, guardSession string) error {
	return a.rpc(r, 20*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		id := r.PathValue("id")
		if guardSession != "" {
			s, err := c.Snapshot(ctx, id)
			if err != nil {
				return err
			}
			if s.State.SessionID != guardSession {
				return errors.New("This instance switched sessions. Open the active session first.")
			}
		}
		_, err := workerCommand(ctx, c, id, name, args)
		return err
	})
}

func workerCommand(ctx context.Context, c *client.CoordinatedClient, id, name string, args any) (*client.Response, error) {
	reply, err := c.Command(ctx, id, client.Command{Type: name, Args: args})
	if err != nil || reply.OK {
		return reply, err
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return nil, errors.New(reply.Error.Message)
	}
	return nil, errors.New("Tau refused the request.")
}

func (a app) prompt(w http.ResponseWriter, r *http.Request) {
	if err := parsePromptForm(w, r); err != nil {
		result(w, "", err)
		return
	}
	if r.MultipartForm != nil {
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("remove temporary attachments: %v", err)
			}
		}()
	}
	text, session := r.PostForm.Get("text"), r.PostForm.Get("session")
	behavior := r.PostForm.Get("behavior")
	if behavior == "" {
		behavior = "steer"
	}
	if session == "" {
		result(w, "", errors.New("This page hasn't loaded the session yet. Wait for the conversation to load, then send again."))
		return
	}
	if behavior != "steer" && behavior != "follow_up" {
		result(w, "", fmt.Errorf("Unknown send action %q.", behavior))
		return
	}
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["attachment"]
	}
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		result(w, "", errors.New("Enter a message or attach a file."))
		return
	}
	err := a.rpc(r, 25*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		id := r.PathValue("id")
		ids, err := uploadAttachments(ctx, c, id, files)
		if err != nil {
			if errors.Is(err, client.ErrOutcomeUnknown) {
				return errors.New("Could not confirm the attachment upload. The prompt was not sent.")
			}
			return err
		}
		args := map[string]any{"text": text, "expected_session_id": session, "streaming_behavior": behavior}
		if len(ids) != 0 {
			args["attachment_ids"] = ids
		}
		_, err = workerCommand(ctx, c, id, "send_message", args)
		if errors.Is(err, coord.ErrFrameTooLarge) {
			return errors.New("This message is larger than Tau's 1 MiB message limit, so it wasn't sent. Attach the text as a file instead.")
		}
		return err
	})
	if err != nil {
		result(w, "", err)
		return
	}
	w.Header().Set("HX-Trigger", "promptAccepted")
	w.WriteHeader(http.StatusOK)
}

func (a app) editQueue(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	session, id, text := r.PostForm.Get("session"), r.PostForm.Get("id"), r.PostForm.Get("text")
	if session == "" || id == "" || strings.TrimSpace(text) == "" {
		result(w, "", errors.New("Choose a queued message and enter its new text."))
		return
	}
	args := map[string]string{"expected_session_id": session, "id": id, "text": text}
	err := a.command(r, "edit_queued_text", args, "")
	if err != nil {
		result(w, "", err)
		return
	}
	w.Header().Set("HX-Trigger", "queueEditSaved")
	w.WriteHeader(http.StatusOK)
}

func (a app) interrupt(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	session := r.PostForm.Get("session")
	if session == "" {
		result(w, "", errors.New("Choose a connected session first."))
		return
	}
	err := a.command(r, "interrupt", nil, session)
	result(w, "Interrupt requested.", err)
}

func (a app) answer(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	f := r.PostForm
	id, method := f.Get("request"), f.Get("method")
	if id == "" || (method != "confirm" && method != "select" && method != "input") {
		result(w, "", errors.New("This request can't be answered."))
		return
	}
	args := map[string]any{"id": id, "method": method}
	switch f.Get("action") {
	case "approve", "reject":
		args["confirmed"] = f.Get("action") == "approve"
	case "cancel":
		args["cancelled"] = true
	default:
		args["value"] = f.Get("value")
	}
	result(w, "Response sent.", a.command(r, "ui_response", args, ""))
}

func (a app) start(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	cwd := strings.TrimSpace(r.PostForm.Get("cwd"))
	if !filepath.IsAbs(cwd) || len(cwd) > 4096 {
		result(w, "", errors.New("Enter an absolute project path on the Tau host."))
		return
	}
	var inst *coord.Instance
	err := a.rpc(r, 22*time.Second, func(ctx context.Context, c *client.CoordinatedClient) (err error) {
		inst, err = c.StartInstance(ctx, cwd)
		return err
	})
	if err != nil {
		result(w, "", err)
		return
	}
	w.Header().Set("HX-Redirect", "/worker/"+url.PathEscape(inst.ID))
	w.WriteHeader(http.StatusOK)
}
