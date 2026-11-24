package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
)

// workerState is get_state. Pointer fields are absent on older workers.
type workerState struct {
	SessionID      string  `json:"session_id"`
	Model          string  `json:"model"`
	ThinkingLevel  string  `json:"thinking_level"`
	IsStreaming    bool    `json:"is_streaming"`
	AutoCompaction *bool   `json:"auto_compaction"`
	AutoRetry      *bool   `json:"auto_retry"`
	SteeringMode   string  `json:"steering_mode"`
	FollowUpMode   string  `json:"follow_up_mode"`
	SessionName    *string `json:"session_name"`
}

func (s workerState) RetryKnown() bool { return s.AutoRetry != nil }
func (s workerState) RetryOn() bool    { return s.AutoRetry != nil && *s.AutoRetry }

func (s workerState) Name() string {
	if s.SessionName == nil {
		return ""
	}
	return *s.SessionName
}

type modelOption struct {
	ID             string   `json:"id"`
	Provider       string   `json:"provider"`
	Name           string   `json:"name"`
	ContextWindow  int      `json:"context_window"`
	Reasoning      bool     `json:"reasoning"`
	ThinkingLevels []string `json:"thinking_levels"`
	Scoped         bool     `json:"-"`
}

// Key is the provider/id form that set_model accepts.
func (m modelOption) Key() string {
	if m.Provider == "" || strings.HasPrefix(m.ID, m.Provider+"/") {
		return m.ID
	}
	return m.Provider + "/" + m.ID
}

type savedSession struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	LastModified string `json:"last_modified"`
	MessageCount int    `json:"message_count"`
	Pinned       bool   `json:"pinned"`
}

func (s savedSession) When() string {
	t, err := time.Parse(time.RFC3339Nano, s.LastModified)
	if err != nil {
		return s.LastModified
	}
	t = t.Local()
	switch now := time.Now(); {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "Today, " + t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2, 15:04")
	default:
		return t.Format("Jan 2, 2006")
	}
}

type mcpServer struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	ToolCount int    `json:"tool_count"`
}

type pluginEntry struct {
	Name    string `json:"name"`
	Plugin  string `json:"plugin"`
	Enabled bool   `json:"enabled"`
	Loaded  bool   `json:"loaded"`
}

type pickEntry struct {
	EntryID string `json:"entry_id"`
	Role    string `json:"role"`
	Text    string `json:"text"`
}

type controlsView struct {
	ID, Session, Tab, Error, Notice string
	State                           workerState
	InstanceName                    string
	Stats                           *sdk.SessionStats
	// Model tab.
	Models         []modelOption
	PinnedModel    string
	Levels         []string
	HasScope       bool
	ThinkingUnsure bool
	// History tab.
	Sessions []savedSession
	// Tools tab.
	MCP                        []mcpServer
	Plugins                    []pluginEntry
	MCPMissing, PluginsMissing bool
	// Fork or forget picker.
	Pick    string
	Entries []pickEntry
}

func (v controlsView) ContextText() string {
	if v.Stats == nil || v.Stats.ContextWindow == 0 {
		return "—"
	}
	return fmt.Sprintf("%s / %s", tokens(v.Stats.TotalTokens), tokens(v.Stats.ContextWindow))
}

func (v controlsView) Cost() string {
	if v.Stats == nil {
		return "—"
	}
	return fmt.Sprintf("$%.2f", v.Stats.Usage.Cost.Total)
}

func tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprint(n)
	}
}

// call runs a worker command and decodes its result into out, if non-nil.
func call(ctx context.Context, c *client.CoordinatedClient, worker, name string, args, out any) error {
	if args == nil {
		args = struct{}{}
	}
	reply, err := workerCommand(ctx, c, worker, name, args)
	if err != nil || out == nil {
		return err
	}
	data, err := json.Marshal(reply.Result)
	if err != nil {
		return fmt.Errorf("read %s reply: %w", name, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("read %s reply: %w", name, err)
	}
	return nil
}

// unsupported reports a command this worker doesn't have yet.
func unsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown command") || strings.Contains(msg, "not supported") || strings.Contains(msg, "unknown_command")
}

func loadControls(ctx context.Context, c *client.CoordinatedClient, v *controlsView) error {
	if err := call(ctx, c, v.ID, "get_state", nil, &v.State); err != nil {
		return err
	}
	if v.Session != "" && v.State.SessionID != "" && v.State.SessionID != v.Session {
		return errors.New("This instance switched sessions. Reload the conversation to continue.")
	}
	switch v.Tab {
	case "model":
		return loadModels(ctx, c, v)
	case "sessions":
		var list struct {
			Sessions []savedSession `json:"sessions"`
		}
		if err := call(ctx, c, v.ID, "list_sessions", map[string]int{"limit": 50}, &list); err != nil {
			return err
		}
		v.Sessions = list.Sessions
		slices.SortStableFunc(v.Sessions, func(a, b savedSession) int {
			if a.Pinned != b.Pinned {
				if a.Pinned {
					return -1
				}
				return 1
			}
			return cmp.Compare(b.LastModified, a.LastModified)
		})
	case "tools":
		var servers struct {
			Servers []mcpServer `json:"servers"`
		}
		err := call(ctx, c, v.ID, "get_mcp_servers", nil, &servers)
		v.MCPMissing = unsupported(err)
		if err != nil && !v.MCPMissing {
			return err
		}
		v.MCP = servers.Servers
		var plugins struct {
			Entries []pluginEntry `json:"entries"`
		}
		err = call(ctx, c, v.ID, "get_plugins", nil, &plugins)
		v.PluginsMissing = unsupported(err)
		if err != nil && !v.PluginsMissing {
			return err
		}
		// Plugins are listed; their individual tools are not.
		for _, p := range plugins.Entries {
			if p.Plugin == "" {
				v.Plugins = append(v.Plugins, p)
			}
		}
	default:
		v.Tab = "session"
		var stats sdk.SessionStats
		if err := call(ctx, c, v.ID, "get_session_stats", nil, &stats); err != nil {
			return err
		}
		v.Stats = &stats
		items, err := c.ListInstances(ctx)
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(items, func(i coord.Instance) bool { return i.ID == v.ID }); i >= 0 {
			v.InstanceName = items[i].Name
		}
	}
	return nil
}

func loadModels(ctx context.Context, c *client.CoordinatedClient, v *controlsView) error {
	var list struct {
		Models []modelOption `json:"models"`
	}
	if err := call(ctx, c, v.ID, "get_available_models", nil, &list); err != nil {
		return err
	}
	v.Models = list.Models
	var scope struct {
		Patterns []string `json:"patterns"`
		Models   []struct {
			Model string `json:"model"`
		} `json:"models"`
	}
	err := call(ctx, c, v.ID, "get_scoped_models", nil, &scope)
	if err != nil && !unsupported(err) {
		return err
	}
	if err == nil && scope.Patterns != nil {
		v.HasScope = true
		for i := range v.Models {
			key := v.Models[i].Key()
			v.Models[i].Scoped = slices.ContainsFunc(scope.Models, func(s struct {
				Model string `json:"model"`
			},
			) bool {
				return s.Model == key
			})
		}
	}
	slices.SortStableFunc(v.Models, func(a, b modelOption) int {
		return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Key(), b.Key()))
	})
	// Older workers don't report per-model levels; offer the whole ladder.
	v.Levels, v.ThinkingUnsure = ai.ThinkingLevels, true
	for _, m := range v.Models {
		if m.Key() == v.State.Model && m.ThinkingLevels != nil {
			v.Levels, v.ThinkingUnsure = m.ThinkingLevels, false
		}
	}
	return nil
}

// pinModel moves one model to the top without changing the order of the others.
func (v *controlsView) pinModel(key string) {
	v.PinnedModel = key
	if i := slices.IndexFunc(v.Models, func(m modelOption) bool { return m.Key() == key }); i > 0 {
		model := v.Models[i]
		copy(v.Models[1:i+1], v.Models[:i])
		v.Models[0] = model
	}
}

func (v controlsView) NoThinking() bool {
	return !v.ThinkingUnsure && (len(v.Levels) == 0 || slices.Equal(v.Levels, []string{"off"}))
}

func (a app) controls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := controlsView{ID: r.PathValue("id"), Session: q.Get("session"), Tab: q.Get("tab")}
	err := a.rpc(r, 8*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		return loadControls(ctx, c, &v)
	})
	if err != nil {
		v.Error = err.Error()
	}
	if v.Tab == "model" {
		v.pinModel(v.State.Model)
	}
	render(w, "controls-body", v)
}

func (a app) controlsPick(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := controlsView{ID: r.PathValue("id"), Session: q.Get("session"), Tab: "session", Pick: q.Get("kind")}
	name := map[string]string{"fork": "get_fork_entries", "forget": "get_forget_entries"}[v.Pick]
	if name == "" {
		http.Error(w, "Unknown picker", http.StatusBadRequest)
		return
	}
	err := a.rpc(r, 8*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		var out struct {
			Entries []pickEntry `json:"entries"`
		}
		err := call(ctx, c, v.ID, name, map[string]string{"expected_session_id": v.Session}, &out)
		if unsupported(err) {
			return errors.New("This Tau worker can't list fork points yet. Update Tau and restart the instance.")
		}
		v.Entries = out.Entries
		return err
	})
	if err != nil {
		v.Error = err.Error()
	}
	render(w, "controls-body", v)
}

// controlAction runs one panel action and re-renders the tab it came from.
func (a app) controlAction(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	f := r.PostForm
	v := controlsView{ID: r.PathValue("id"), Session: f.Get("session"), Tab: f.Get("tab")}
	action, value := f.Get("action"), f.Get("value")
	enabled := f.Get("enabled") == "on"
	// A command run from the composer reports in the composer's notice line.
	inline := f.Get("inline") != ""
	var (
		name     string
		args     any
		redirect bool
		guard    bool
		timeout  = 20 * time.Second
	)
	switch action {
	case "model":
		name, args = "set_model", map[string]string{"model_spec": value}
	case "thinking":
		name, args = "set_thinking_level", map[string]string{"level": value}
	case "auto_compaction", "auto_retry":
		name, args = "set_"+action, map[string]bool{"enabled": enabled}
	case "steering_mode", "follow_up_mode":
		name, args = "set_"+action, map[string]string{"mode": value}
	case "name":
		if strings.TrimSpace(value) == "" {
			v.Error = "Enter a name for this session."
			break
		}
		// The command has no session guard, so check before renaming.
		name, args, guard, v.Notice = "set_session_name", map[string]string{"name": strings.TrimSpace(value)}, true, "Renamed."
	case "instance_name":
		// An empty name clears it. The name belongs to the worker, so it
		// needs no session guard.
		clean, err := coord.NormalizeInstanceName(value)
		if err != nil {
			v.Error = fmt.Sprintf("Use one line of at most %d bytes for the instance name.", coord.MaxInstanceNameBytes)
			break
		}
		name, value, v.Notice = action, clean, "Instance renamed."
		if clean == "" {
			v.Notice = "Instance name cleared."
		}
	case "compact":
		// Compaction replies only once it finishes.
		name, args, guard, timeout, v.Notice = "compact", map[string]string{"custom_instructions": value}, true, 10*time.Minute, "History compacted."
	case "retry":
		name, args, v.Notice = "send_message", map[string]string{"text": "/retry", "expected_session_id": v.Session, "streaming_behavior": "steer"}, "Retrying the last run."
	case "reload":
		name, args, v.Notice = "slash_command", map[string]string{"name": "reload", "args": ""}, "Reloaded plugins and resources."
	case "new":
		name, args, guard, redirect = "new_session", struct{}{}, true, true
	case "resume":
		if value == "" {
			v.Error = "Choose a session to resume."
			break
		}
		name, args, guard, redirect = "resume_session", map[string]string{"id": value}, true, true
	case "pin", "unpin":
		name, args = action+"_session", map[string]string{"id": value}
	case "fork":
		name, args, guard, redirect = "fork", map[string]string{"entry_id": value}, true, true
	case "forget":
		name, args, v.Notice = "forget", map[string]string{"expected_session_id": v.Session, "entry_id": value}, "Dropped those messages."
	case "mcp_toggle":
		name, args = "mcp_configure", map[string]any{"name": value, "enabled": enabled}
	case "mcp_reconnect", "mcp_sign_out":
		name, args = action, map[string]string{"name": value}
	case "mcp_sign_in":
		name, args, v.Notice = action, map[string]string{"name": value}, "Sign-in started. Follow the request in the conversation."
	case "plugin_toggle":
		name, args = "disable_plugin", map[string]string{"name": value}
		if enabled {
			name = "enable_plugin"
		}
	default:
		v.Error = fmt.Sprintf("Unknown action %q.", action)
	}
	err := a.rpc(r, timeout+10*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		if name != "" {
			if guard && v.Session != "" {
				s, err := c.Snapshot(ctx, v.ID)
				if err != nil {
					return err
				}
				if s.State.SessionID != v.Session {
					return errors.New("This instance switched sessions. Reload the conversation first.")
				}
			}
			if action == "resume" && c.CanLocateSession() {
				target, err := resumeElsewhere(ctx, c, v.ID, value)
				if err != nil {
					return err
				}
				if target != "" {
					v.ID = target
					return nil
				}
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			var err error
			if action == "instance_name" {
				err = c.RenameInstance(cctx, v.ID, value)
			} else {
				err = call(cctx, c, v.ID, name, args, nil)
			}
			cancel()
			switch {
			case errors.Is(err, client.ErrOutcomeUnknown):
				err = errors.New("Tau may have applied this. Check before trying again; nothing was resent.")
			case unsupported(err):
				err = errors.New("This Tau worker doesn't support that yet. Update Tau and restart the instance.")
			}
			if inline || (redirect && err == nil) {
				return err
			}
			if err != nil {
				v.Error, v.Notice = err.Error(), ""
			}
		}
		if inline {
			return nil
		}
		// Reload after a failure too, so the error doesn't blank the panel.
		failed := v.Error
		err := loadControls(ctx, c, &v)
		if failed != "" {
			v.Error = failed
			return nil
		}
		return err
	})
	if err != nil {
		v.Error, v.Notice = err.Error(), ""
	}
	if redirect && err == nil && v.Error == "" {
		w.Header().Set("HX-Redirect", "/worker/"+v.ID)
		return
	}
	if v.Error == "" {
		trigger := "conversationRefresh"
		if action == "name" || action == "instance_name" {
			trigger += ", instanceListRefresh"
		}
		w.Header().Set("HX-Trigger", trigger)
	}
	if inline {
		// A card shows its result in the list, so only a failure needs a notice.
		if f.Get("inline") == "card" {
			v.Notice = ""
		}
		render(w, "notice", notice{Text: cmp.Or(v.Error, v.Notice), Error: v.Error != ""})
		return
	}
	if v.Tab == "model" {
		// Keep the list steady until the user comes back to the tab.
		v.pinModel(f.Get("pinned_model"))
	}
	render(w, "controls-body", v)
}

// exportHTML has Tau write the export on this host, then sends it to the browser.
func (a app) exportHTML(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "tau-ui-export-")
	if err != nil {
		http.Error(w, "Could not prepare the export.", http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("remove export: %v", err)
		}
	}()
	var out struct {
		Path string `json:"path"`
	}
	err = a.rpc(r, 60*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		return call(ctx, c, r.PathValue("id"), "export_html", map[string]string{"output_path": filepath.Join(dir, "session.html")}, &out)
	})
	if err != nil {
		http.Error(w, "Export failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	data, err := os.ReadFile(out.Path)
	if err != nil {
		http.Error(w, "Tau exported the session, but Tau UI couldn't read the file. Tau UI must run as the same user on the same host.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tau-session.html"`)
	// The export is a standalone document; don't apply this app's policy to it.
	w.Header().Set("Content-Security-Policy", "sandbox")
	if _, err := w.Write(data); err != nil {
		log.Printf("send export: %v", err)
	}
}

func (a app) lastReply(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Text string `json:"text"`
	}
	err := a.rpc(r, 8*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		return call(ctx, c, r.PathValue("id"), "get_last_assistant_text", nil, &out)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := w.Write([]byte(out.Text)); err != nil {
		log.Printf("send last reply: %v", err)
	}
}

type queueModeView struct {
	View                       controlsView
	Action, Title, Hint, Value string
}

type controlButtonView struct {
	View                                 controlsView
	Action, Value, Label, Confirm, Class string
}

var linkPattern = regexp.MustCompile(`https?://[^\s<>"]+[^\s<>".,;:!?)\]]`)

// linkify escapes text and turns its web addresses into links that open in a
// new tab, such as an MCP sign-in page in an input request.
func linkify(text string) template.HTML {
	var out strings.Builder
	last := 0
	for _, m := range linkPattern.FindAllStringIndex(text, -1) {
		out.WriteString(template.HTMLEscapeString(text[last:m[0]]))
		url := template.HTMLEscapeString(text[m[0]:m[1]])
		fmt.Fprintf(&out, `<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>`, url, url)
		last = m[1]
	}
	out.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(out.String())
}

// controlFuncs are the template helpers for the controls panel.
var controlFuncs = map[string]any{
	"tokens":  tokens,
	"linkify": linkify,
	"queueMode": func(v controlsView, action, title, hint, value string) queueModeView {
		return queueModeView{v, action, title, hint, value}
	},
	"controlButton": func(v controlsView, action, value, label, confirm string) controlButtonView {
		class := map[string]string{"new": "danger-outline", "forget": "danger-outline", "resume": "primary"}[action]
		return controlButtonView{v, action, value, label, confirm, class}
	},
	"mcpStatus": func(status string) string {
		return map[string]string{
			"connected": "Connected", "connecting": "Connecting…", "disabled": "Disabled",
			"failed": "Failed", "needs-auth": "Sign-in needed", "closed": "Closed",
		}[status]
	},
	"pickLabel": func(kind string) string {
		return map[string]string{"fork": "Fork here", "forget": "Forget from here"}[kind]
	},
	"pickConfirm": func(kind string) string {
		return map[string]string{
			"fork":   "Start a new session that branches at this message?",
			"forget": "Remove this message and everything after it from the session? This can't be undone.",
		}[kind]
	},
}

type paletteCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// commands lists the worker's skill, prompt, and plugin commands for the
// composer's command menu. The menu adds its own built-in entries.
func (a app) commands(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Commands []paletteCommand `json:"commands"`
	}
	err := a.rpc(r, 8*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		return call(ctx, c, r.PathValue("id"), "get_commands", nil, &out)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	extra := slices.DeleteFunc(out.Commands, func(c paletteCommand) bool { return c.Source == "builtin" || c.Name == "" })
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(extra); err != nil {
		log.Printf("send commands: %v", err)
	}
}
