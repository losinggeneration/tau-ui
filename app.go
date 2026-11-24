package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
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
	socket      string
	inbox       *inboxOrder
	restarts    *restartWatch
	projectRoot string
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
	Label, Badge, Limit        string
	Active, Restart, Dismissed bool
}

// DisplayName is the session name, or the project directory without one.
func (i instance) DisplayName() string {
	return cmp.Or(i.SessionName, projectName(i.CWD))
}

// Running reports whether a restart could interrupt a run. An unknown state
// counts because nothing says the worker is idle.
func (i instance) Running() bool {
	return i.Badge == "unknown" || i.Instance.Active()
}

// Updatable reports whether the card can offer a restart onto the current build.
func (i instance) Updatable() bool {
	return i.Outdated && i.Restart && i.SessionID != "" && i.Badge != "offline"
}

var stateBadges = map[string]string{
	coord.StateAwaitingUser: "ready",
	coord.StateWorking:      "busy",
	coord.StateNeedsInput:   "waiting",
	coord.StateUsageLimited: "limited",
	coord.StateRetrying:     "retrying",
	coord.StatePaused:       "paused",
	coord.StateError:        "error",
}

func summary(i coord.Instance) instance {
	out := instance{Instance: i, Label: i.StatusText(), Badge: stateBadges[i.State], Limit: limitKey(i)}
	switch {
	case i.LinkState == coord.LinkExited:
		out.Label, out.Badge = "exited", "offline"
	case i.LinkState != "" && i.LinkState != coord.LinkConnected:
		out.Label, out.Badge = "disconnected", "offline"
	case i.PendingInputs > 0:
		out.Badge = "waiting"
	case out.Badge == "":
		out.Label, out.Badge = "unknown", "unknown"
		if i.State != "" {
			out.Label += ": " + i.State
		}
	}
	return out
}

type instanceGroup struct {
	Key, Title        string
	Collapsible, Open bool
	Items             []instance
}

type registry struct {
	Error   string
	Groups  []instanceGroup
	Current instance
	// Live marks a poll, which also updates the open conversation's heading.
	Live bool
}

// NoneRunning reports whether only exited workers, or none at all, are listed.
func (v registry) NoneRunning() bool {
	return !slices.ContainsFunc(v.Groups, func(g instanceGroup) bool { return g.Key != "done" })
}

// groupInstances returns the sidebar groups in display order, without empty ones.
func groupInstances(items []instance) []instanceGroup {
	const (
		attention = iota
		input
		working
		waiting
		disconnected
		done
	)
	groups := [...]instanceGroup{
		attention:    {Key: "attention", Title: "Needs attention"},
		input:        {Key: "input", Title: "Needs input"},
		working:      {Key: "working", Title: "Working", Collapsible: true, Open: true},
		waiting:      {Key: "waiting", Title: "Waiting", Collapsible: true, Open: true},
		disconnected: {Key: "disconnected", Title: "Disconnected"},
		done:         {Key: "done", Title: "Done", Collapsible: true},
	}
	for _, i := range items {
		at := input
		switch {
		case i.LinkState == coord.LinkExited:
			at = done
		case i.Badge == "offline":
			at = disconnected
		case i.Badge == "error", i.Badge == "unknown", i.Badge == "limited" && !i.Dismissed:
			at = attention
		case i.Badge == "limited", i.Badge == "paused":
			at = waiting
		case i.Badge == "busy", i.Badge == "retrying":
			at = working
		}
		groups[at].Items = append(groups[at].Items, i)
	}
	return slices.DeleteFunc(groups[:], func(g instanceGroup) bool { return len(g.Items) == 0 })
}

func (a app) loadRegistry(ctx context.Context, c *client.CoordinatedClient, active string) (registry, error) {
	var v registry
	restart := restartSupported(c)
	items, err := c.ListInstances(ctx)
	if err != nil {
		return v, err
	}
	sortTimes := a.inbox.sortTimes(c.Generation(), items)
	slices.SortFunc(items, func(a, b coord.Instance) int {
		return cmp.Or(sortTimes[b.ID].Compare(sortTimes[a.ID]), strings.Compare(a.ID, b.ID))
	})
	dismissed := a.inbox.dismissedLimits(items)
	cards := make([]instance, 0, len(items))
	for _, item := range items {
		i := summary(item)
		i.Active, i.Restart, i.Dismissed = i.ID == active, restart, dismissed[i.ID]
		if i.Active {
			v.Current = i
		}
		cards = append(cards, i)
	}
	v.Groups = groupInstances(cards)
	return v, nil
}

func (a app) instances(w http.ResponseWriter, r *http.Request) {
	active := r.URL.Query().Get("active")
	var v registry
	err := a.rpc(r, 4*time.Second, func(ctx context.Context, c *client.CoordinatedClient) (err error) {
		v, err = a.loadRegistry(ctx, c, active)
		return err
	})
	if err != nil {
		v.Error = err.Error()
	}
	v.Live = true
	render(w, "instances", v)
}

type pageView struct {
	Registry         registry
	ID, Session, CWD string
	Feed             feedView
	Before           string
}

// DisplayName names the open conversation.
func (v pageView) DisplayName() string {
	return cmp.Or(v.Registry.Current.SessionName, projectName(v.CWD))
}

func (a app) page(w http.ResponseWriter, r *http.Request) {
	v := pageView{ID: r.PathValue("id")}
	err := a.rpc(r, 6*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		list, err := a.loadRegistry(ctx, c, v.ID)
		v.Registry = list
		if err != nil {
			return err
		}
		if v.ID == "" {
			return nil
		}
		v.CWD, v.Session = list.Current.CWD, list.Current.SessionID
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
	Steering       int                `json:"steering"`
	FollowUp       int                `json:"follow_up"`
	SteeringPaused bool               `json:"steering_paused"`
	FollowUpPaused bool               `json:"follow_up_paused"`
	Items          []sdk.QueuedPrompt `json:"items"`
}

func (q queueView) Total() int { return q.Steering + q.FollowUp }

func (q queueView) Paused() bool { return q.SteeringPaused || q.FollowUpPaused }

// EditableCount decides whether the panel offers Edit all.
func (q queueView) EditableCount() int {
	n := 0
	for _, item := range q.Items {
		if item.Editable {
			n++
		}
	}
	return n
}

type feedView struct {
	ID, Session, Before, Error string
	Instance                   instance
	Messages                   []message
	Requests                   []dialog
	Queue                      queueView
	Busy, Changed              bool
	UsageWait                  *usageLimitWait
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
	v.UsageWait = live.UsageLimitWait
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
	var next, state string
	err := a.rpc(r, 4*time.Second, func(ctx context.Context, c *client.CoordinatedClient) (err error) {
		v, err = snapshot(ctx, c, id, expected)
		if err != nil {
			next, state = a.restarts.fate(ctx, c, id, expected)
			return err
		}
		a.restarts.view(id)
		return nil
	})
	if err != nil {
		if next != "" {
			w.Header().Set(replacementHeader, next)
		}
		if state != "" {
			w.Header().Set(instanceStateHeader, state)
		}
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
		v = failure(err)
	}
	render(w, "notice", v)
}

func failure(err error) notice {
	if errors.Is(err, client.ErrOutcomeUnknown) {
		return notice{Text: "Tau may have received this. Check the conversation or instance list before trying again. Nothing was resent.", Error: true}
	}
	return notice{Text: err.Error(), Error: true}
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
		return nil, tauError{reply.Error.Code, reply.Error.Message}
	}
	return nil, errors.New("Tau refused the request.")
}

// tauError keeps a worker's error code for callers that handle it specially.
type tauError struct{ Code, Message string }

func (e tauError) Error() string { return e.Message }

func hasTauCode(err error, code string) bool {
	e, ok := errors.AsType[tauError](err)
	return ok && e.Code == code
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

// queueKinds is in delivery order. Resuming steering first stops an idle
// session from running follow-ups ahead of it.
var queueKinds = []string{"steer", "follow_up"}

type queueEditRow struct {
	sdk.QueuedPrompt
	Note string
}

// queueEditor is the edit dialog. Resume names only the queues this dialog
// paused, so closing it can't release a pause someone else set.
type queueEditor struct {
	Worker, Session      string
	Resume               []string
	Rows                 []queueEditRow
	Partial, CannotPause bool
	Notice               notice
}

func (v queueEditor) CanSave() bool {
	return slices.ContainsFunc(v.Rows, func(row queueEditRow) bool { return row.Editable })
}

func queueState(ctx context.Context, c *client.CoordinatedClient, id, session string) (queueView, error) {
	var q queueView
	s, err := c.SnapshotPreview(ctx, id)
	if err != nil {
		return q, err
	}
	if s.State.SessionID != session {
		return q, errors.New("This instance switched sessions. Open the active session first.")
	}
	if len(s.State.Queue) > 0 {
		if err := json.Unmarshal(s.State.Queue, &q); err != nil {
			return q, fmt.Errorf("read Tau queue: %w", err)
		}
	}
	return q, nil
}

func resumeQueues(ctx context.Context, c *client.CoordinatedClient, id, session string, queues []string) error {
	for _, kind := range queueKinds {
		if !slices.Contains(queues, kind) {
			continue
		}
		args := map[string]string{"expected_session_id": session, "queue": kind}
		if _, err := workerCommand(ctx, c, id, "resume_queue", args); err != nil {
			return err
		}
	}
	return nil
}

// pauseQueue holds both queues and opens the editor on one message, or on
// every message when no id is given.
func (a app) pauseQueue(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	worker, session, only := r.PathValue("id"), r.PostForm.Get("session"), r.PostForm.Get("id")
	v := queueEditor{Worker: worker, Session: session}
	err := a.rpc(r, 20*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		q, err := queueState(ctx, c, worker, session)
		if err != nil {
			return err
		}
		for _, kind := range queueKinds {
			if kind == "steer" && q.SteeringPaused || kind == "follow_up" && q.FollowUpPaused {
				continue
			}
			args := map[string]string{"expected_session_id": session, "queue": kind}
			if _, err := workerCommand(ctx, c, worker, "pause_queue", args); hasTauCode(err, "unknown_command") {
				v.CannotPause = true
				break
			} else if err != nil {
				return errors.Join(err, resumeQueues(ctx, c, worker, session, v.Resume))
			}
			v.Resume = append(v.Resume, kind)
		}
		// Read again now that nothing can leave: the click may have raced delivery.
		if q, err = queueState(ctx, c, worker, session); err != nil {
			return errors.Join(err, resumeQueues(ctx, c, worker, session, v.Resume))
		}
		for _, item := range q.Items {
			if only != "" && item.ID != only {
				continue
			}
			row := queueEditRow{QueuedPrompt: item}
			if !item.Editable {
				row.Note = "This message can't be edited here."
			}
			v.Rows = append(v.Rows, row)
		}
		v.Partial = only == "" && q.Total() > len(q.Items)
		if !v.CanSave() {
			return errors.Join(errors.New("That message has left the queue or can't be edited here."), resumeQueues(ctx, c, worker, session, v.Resume))
		}
		return nil
	})
	if err != nil {
		w.Header().Set("HX-Retarget", "#notice")
		result(w, "", err)
		return
	}
	w.Header().Set("HX-Trigger-After-Settle", "queueEditOpen")
	render(w, "queue-editor", v)
}

// resumeQueue resumes the named queues, or every paused queue when none is named.
func (a app) resumeQueue(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	worker, session, queues := r.PathValue("id"), r.PostForm.Get("session"), r.PostForm["queue"]
	err := a.rpc(r, 20*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		if len(queues) == 0 {
			q, err := queueState(ctx, c, worker, session)
			if err != nil {
				return err
			}
			if q.SteeringPaused {
				queues = append(queues, "steer")
			}
			if q.FollowUpPaused {
				queues = append(queues, "follow_up")
			}
		}
		return resumeQueues(ctx, c, worker, session, queues)
	})
	result(w, "Queue resumed.", err)
}

// editQueue saves the dialog and resumes what it paused. Tau replaces a
// message's ID on each edit, so after a partial failure the dialog keeps only
// the unsaved rows; a retry can't target an ID that's already gone.
func (a app) editQueue(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	worker, session, ids, texts := r.PathValue("id"), r.PostForm.Get("session"), r.PostForm["id"], r.PostForm["text"]
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	if session == "" || len(ids) == 0 || len(ids) != len(texts) || slices.ContainsFunc(ids, blank) || slices.ContainsFunc(texts, blank) {
		w.Header().Set("HX-Retarget", "#queue-edit-notice")
		result(w, "", errors.New("Choose a queued message and enter its new text."))
		return
	}
	lf := strings.NewReplacer("\r\n", "\n")
	v := queueEditor{Worker: worker, Session: session, Resume: r.PostForm["resume"]}
	for i, id := range ids {
		v.Rows = append(v.Rows, queueEditRow{QueuedPrompt: sdk.QueuedPrompt{ID: id, Text: lf.Replace(texts[i]), Editable: true}})
	}
	const staleNote = "This message changed or left the queue. Copy your text if you still need it."
	saved := 0
	err := a.rpc(r, 20*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		q, err := queueState(ctx, c, worker, session)
		if err != nil {
			return err
		}
		current := make(map[string]sdk.QueuedPrompt, len(q.Items))
		for _, item := range q.Items {
			current[item.ID] = item
		}
		stale := false
		for i := range v.Rows {
			row := &v.Rows[i]
			item, ok := current[row.ID]
			row.Kind = item.Kind
			if !ok || !item.Editable {
				row.Editable, row.Note, stale = false, staleNote, true
			}
		}
		if stale {
			return errors.New("A message left the queue or changed. Your edit was not sent.")
		}
		for len(v.Rows) > 0 {
			row := &v.Rows[0]
			if row.Text != lf.Replace(current[row.ID].Text) {
				args := map[string]string{"expected_session_id": session, "id": row.ID, "text": row.Text}
				if _, err := workerCommand(ctx, c, worker, "edit_queued_text", args); err != nil {
					if hasTauCode(err, "queue_changed") || errors.Is(err, client.ErrOutcomeUnknown) {
						row.Editable, row.Note = false, staleNote
					}
					return err
				}
				saved++
			}
			v.Rows = v.Rows[1:]
		}
		return resumeQueues(ctx, c, worker, session, v.Resume)
	})
	if err != nil && len(v.Rows) > 0 {
		v.Notice = failure(err)
		if saved > 0 {
			v.Notice.Text = fmt.Sprintf("Saved %d. %s", saved, v.Notice.Text)
		}
		render(w, "queue-editor", v)
		return
	}
	w.Header().Set("HX-Trigger", "queueEditSaved")
	if err != nil {
		log.Printf("resume queue after edit: %v", err)
		w.Header().Set("HX-Retarget", "#notice")
		render(w, "notice", notice{Text: "Your edits were saved, but Tau didn't confirm the queue resumed. Select Resume if it still shows as paused.", Error: true})
		return
	}
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

const (
	maxProjectPath     = 4096
	projectSearchLimit = 1000
	// Search the most recently changed top-level folders first.
	projectSearchRecent = 10
	// Levels below each top-level folder searched before going deeper anywhere.
	projectSearchEarlyDepth = 3
)

type projectSearchDir struct {
	path    string
	level   int // 0 for a direct child of the search folder.
	modTime time.Time
}

type projectDir struct {
	Name, Relative string
}

// projectDirOptions are the picker's advanced search settings.
type projectDirOptions struct {
	Hidden bool // Browse and search dot-folders.
	Deps   bool // Search dependency and build folders.
}

type projectDirsView struct {
	projectDirOptions
	Path, Parent, Dir, Query, Error string
	Dirs                            []projectDir
	Limited, Incomplete             bool
	InHidden                        bool // The current folder is inside a dot-folder.
}

// Search skips these unless asked. Browsing still lists them.
var projectDepDirs = []string{"node_modules", "target", "vendor"}

func (o projectDirOptions) browsable(name string) bool {
	return (o.Hidden || !strings.HasPrefix(name, ".")) && !strings.ContainsAny(name, "\x00\r\n")
}

func (a app) projectDirs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := projectDirOptions{Hidden: q.Get("hidden") == "1", Deps: q.Get("deps") == "1"}
	v, err := a.readProjectDirs(r.Context(), cmp.Or(q.Get("dir"), "."), strings.TrimSpace(q.Get("q")), opts)
	if err != nil {
		v.Dirs = nil
		v.Error = "Can't browse this folder. Choose a different folder or enter the project path manually."
	}
	render(w, "project-dirs", v)
}

// Folder navigation uses paths relative to the configured browse root.
func (a app) readProjectDirs(ctx context.Context, dir, query string, opts projectDirOptions) (projectDirsView, error) {
	v := projectDirsView{projectDirOptions: opts, Dir: dir, Query: query}
	local, err := filepath.Localize(dir)
	if err != nil {
		return v, fmt.Errorf("invalid project folder: %w", err)
	}
	full := filepath.Join(a.projectRoot, local)
	if len(full) > maxProjectPath {
		return v, errors.New("project path is too long")
	}
	if dir != "." {
		for name := range strings.SplitSeq(dir, "/") {
			if !opts.browsable(name) {
				return v, errors.New("project folder hidden or unsupported")
			}
			v.InHidden = v.InHidden || strings.HasPrefix(name, ".")
		}
		v.Parent = filepath.ToSlash(filepath.Dir(local))
	}
	root, err := os.OpenRoot(a.projectRoot)
	if err != nil {
		return v, fmt.Errorf("open project browse root: %w", err)
	}
	defer func() { _ = root.Close() }()
	folder, err := root.OpenRoot(local)
	if err != nil {
		return v, fmt.Errorf("open project folder: %w", err)
	}
	defer func() { _ = folder.Close() }()
	if query != "" {
		if err := v.search(ctx, folder, full); err != nil {
			return v, fmt.Errorf("search project folders: %w", err)
		}
		v.Path = full
		return v, nil
	}
	entries, err := fs.ReadDir(folder.FS(), ".")
	if err != nil {
		return v, fmt.Errorf("list project folders: %w", err)
	}
	v.Path = full
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && opts.validChild(full, name) {
			rel := filepath.ToSlash(filepath.Join(local, name))
			v.Dirs = append(v.Dirs, projectDir{Name: name, Relative: rel})
		}
	}
	return v, nil
}

func (o projectDirOptions) validChild(parent, name string) bool {
	return o.browsable(name) && fs.ValidPath(name) && len(filepath.Join(parent, name)) <= maxProjectPath
}

func (v *projectDirsView) search(ctx context.Context, folder *os.Root, full string) error {
	remaining := projectSearchLimit
	query := strings.ToLower(v.Query)
	// Read one folder, recording its matches and returning its visible children.
	scan := func(parent string, level int) ([]projectSearchDir, error) {
		dir, err := folder.OpenRoot(parent)
		if err != nil {
			return nil, fmt.Errorf("open search folder: %w", err)
		}
		defer func() { _ = dir.Close() }()
		f, err := dir.Open(".")
		if err != nil {
			return nil, fmt.Errorf("open search listing: %w", err)
		}
		defer func() { _ = f.Close() }()
		base := filepath.Join(full, parent)
		var children []projectSearchDir
		// Read incrementally so no directory is scanned past the shared budget.
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return children, err
			}
			entries, err := f.ReadDir(1)
			if errors.Is(err, io.EOF) {
				return children, nil
			}
			if err != nil {
				return children, fmt.Errorf("read search listing: %w", err)
			}
			entry := entries[0]
			name := entry.Name()
			if !entry.IsDir() || !v.validChild(base, name) || !v.Deps && slices.Contains(projectDepDirs, name) {
				continue
			}
			remaining--
			rel := filepath.Join(parent, name)
			child := projectSearchDir{path: rel, level: level}
			if parent == "." {
				// A folder removed mid-search sorts last and fails its own scan.
				if info, err := entry.Info(); err == nil {
					child.modTime = info.ModTime()
				}
			}
			children = append(children, child)
			if strings.Contains(strings.ToLower(name), query) {
				v.Dirs = append(v.Dirs, projectDir{
					Name: filepath.ToSlash(rel), Relative: filepath.ToSlash(filepath.Join(v.Dir, rel)),
				})
			}
		}
		return children, nil
	}
	top, err := scan(".", 0)
	if err != nil {
		return err
	}
	slices.SortFunc(top, func(a, b projectSearchDir) int {
		return cmp.Or(b.modTime.Compare(a.modTime), strings.Compare(a.path, b.path))
	})
	// Walk breadth-first, deferring anything at maxLevel or below to the last pass.
	var deferred []projectSearchDir
	walk := func(queue []projectSearchDir, maxLevel int) {
		for len(queue) > 0 && remaining > 0 && ctx.Err() == nil {
			next := queue[0]
			queue = queue[1:]
			children, err := scan(next.path, next.level+1)
			if err != nil {
				v.Incomplete = true
			}
			for _, child := range children {
				if child.level < maxLevel {
					queue = append(queue, child)
				} else {
					deferred = append(deferred, child)
				}
			}
		}
	}
	recent := min(projectSearchRecent, len(top))
	walk(top[:recent:recent], projectSearchEarlyDepth)
	walk(top[recent:], projectSearchEarlyDepth)
	pending := deferred
	deferred = nil
	walk(pending, math.MaxInt)
	v.Limited = remaining == 0
	slices.SortFunc(v.Dirs, func(a, b projectDir) int { return strings.Compare(a.Name, b.Name) })
	return ctx.Err()
}

func (a app) start(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	cwd := r.PostForm.Get("cwd")
	if !filepath.IsAbs(cwd) {
		cwd = strings.TrimSpace(cwd)
	}
	if !filepath.IsAbs(cwd) || len(cwd) > maxProjectPath || strings.ContainsRune(cwd, 0) {
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
