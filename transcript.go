package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/sdk"
)

type message struct {
	ID, Role, Text, Full string
	Images               []messageImage
	Live                 bool
	Run                  *execution
}

type messageImage struct{ URL, Alt string }

type contentSource struct{ Worker, Session string }

func (s contentSource) link(entry, kind, part string, index *int) string {
	q := url.Values{"session": {s.Session}, "entry": {entry}}
	if kind != "" {
		q.Set("live", kind)
	}
	if part != "" {
		q.Set("part", part)
	}
	if index != nil {
		q.Set("index", strconv.Itoa(*index))
	}
	return "/worker/" + url.PathEscape(s.Worker) + "/content?" + q.Encode()
}

type liveItem struct {
	ID, Name, Role, Input, Text, Progress string
	Truncated                             bool
}

type liveActivity struct {
	Messages, Tools, Bash []liveItem
	UsageLimitWait        *usageLimitWait `json:"usage_limit_wait"`
}

// usageLimitWait is set while the worker sleeps until a provider limit resets.
type usageLimitWait struct {
	Provider string `json:"provider"`
	ResetsAt string `json:"resets_at"`
}

type argument struct{ Label, Value string }

type execution struct {
	Name, Narration, Command, Output, Status string
	Arguments                                []argument
	InputFull, OutputFull                    string
	ResultOnly, Truncated                    bool
}

func executionInput(name string, input json.RawMessage) *execution {
	run := &execution{Name: name}
	var args map[string]any
	if json.Unmarshal(input, &args) != nil {
		// A live snapshot can include only the tail of a long input.
		run.Truncated = len(input) != 0
		return run
	}
	if command, ok := args["command"].(string); ok {
		run.Command = command
		delete(args, "command")
	}
	var add func(string, any, int)
	add = func(label string, value any, depth int) {
		if len(run.Arguments) >= 32 || depth > 6 {
			run.Truncated = true
			return
		}
		switch value := value.(type) {
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(value)) {
				add(label+" › "+key, value[key], depth+1)
			}
		case []any:
			for i, item := range value {
				add(fmt.Sprintf("%s %d", label, i+1), item, depth+1)
			}
		case nil:
			run.Arguments = append(run.Arguments, argument{label, "—"})
		default:
			run.Arguments = append(run.Arguments, argument{label, fmt.Sprint(value)})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(args)) {
		add(key, args[key], 0)
	}
	return run
}

func contentText(block ai.ContentBlock) string {
	if block.Type == ai.ContentBlockText {
		return block.Text
	}
	return ""
}

func messageText(msg sdk.Message) string {
	if len(msg.Content) == 0 {
		return strings.TrimSpace(msg.Text)
	}
	var out strings.Builder
	for _, block := range msg.Content {
		out.WriteString(contentText(block))
	}
	return strings.TrimSpace(out.String())
}

func imageMarker(text string) (string, bool) {
	const prefix = `<file name=`
	file, ok := strings.CutPrefix(text, prefix)
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(file, `>[image content]</file>`)
	if !ok {
		return "", false
	}
	alt, err := strconv.Unquote(name)
	return alt, err == nil
}

func (src contentSource) userMessage(entry string, msg sdk.Message) message {
	bubble := message{ID: "entry:" + entry, Role: "user"}
	var text []string
	var imageIndex int
	addImage := func(block ai.ContentBlock, alt string) {
		if alt == "" {
			alt = "Attached image"
		}
		if rasterMIME(block.MimeType) {
			bubble.Images = append(bubble.Images, messageImage{src.link(entry, "", "image", &imageIndex), alt})
		}
		imageIndex++
	}
	for i, block := range msg.Content {
		switch block.Type {
		case ai.ContentBlockText:
			if i+1 < len(msg.Content) && msg.Content[i+1].Type == ai.ContentBlockImage {
				if _, marker := imageMarker(block.Text); marker {
					continue
				}
			}
			if strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		case ai.ContentBlockImage:
			var alt string
			if i > 0 {
				alt, _ = imageMarker(msg.Content[i-1].Text)
			}
			addImage(block, alt)
		}
	}
	if imageIndex == 0 {
		for _, block := range msg.Images {
			if block.Type == ai.ContentBlockImage {
				addImage(block, "")
			}
		}
	}
	if len(msg.Content) == 0 {
		bubble.Text = strings.TrimSpace(msg.Text)
	} else {
		bubble.Text = strings.Join(text, "\n\n")
	}
	return bubble
}

func (src contentSource) transcript(entries []client.HistoryEntry, live liveActivity) []message {
	var out []message
	calls := make(map[string]*execution)
	for _, entry := range entries {
		id := "entry:" + entry.EntryID
		if entry.Omitted {
			out = append(out, message{ID: id, Role: "note", Full: src.link(entry.EntryID, "", "", nil)})
			continue
		}
		var msg sdk.Message
		if json.Unmarshal(entry.Message, &msg) != nil {
			continue
		}
		switch msg.Role {
		case "user":
			bubble := src.userMessage(entry.EntryID, msg)
			if entry.Truncated {
				bubble.Full = src.link(entry.EntryID, "", "", nil)
			}
			if bubble.Text != "" || len(bubble.Images) != 0 || bubble.Full != "" {
				out = append(out, bubble)
			}
		case "assistant":
			if len(msg.Content) == 0 {
				bubble := message{ID: id, Role: msg.Role, Text: messageText(msg)}
				if entry.Truncated {
					bubble.Full = src.link(entry.EntryID, "", "", nil)
				}
				if bubble.Text != "" || bubble.Full != "" {
					out = append(out, bubble)
				}
				break
			}
			narration := -1
			for i, block := range msg.Content {
				key := fmt.Sprintf("%s:%d", id, i)
				if block.Type == ai.ContentBlockToolCall {
					run := executionInput(block.Name, block.Arguments)
					if entry.Truncated || run.Truncated {
						run.InputFull = src.link(entry.EntryID, "", "call", &i)
					}
					if narration >= 0 {
						run.Narration = out[narration].Text
						out = out[:narration]
						narration = -1
					}
					if block.ID != "" {
						key = "tool:" + block.ID
						calls[block.ID] = run
					}
					out = append(out, message{ID: key, Run: run})
				} else if text := strings.TrimSpace(contentText(block)); text != "" {
					narration = -1
					if msg.Role == "assistant" && block.Type == ai.ContentBlockText {
						narration = len(out)
					}
					bubble := message{ID: key, Role: msg.Role, Text: text}
					if entry.Truncated {
						bubble.Full = src.link(entry.EntryID, "", "text", &i)
					}
					out = append(out, bubble)
				}
			}
		case "tool", "toolResult":
			run := calls[msg.ToolCallID]
			if run == nil {
				run = &execution{Name: msg.ToolName, ResultOnly: true}
				if msg.ToolCallID != "" {
					id = "tool:" + msg.ToolCallID
					calls[msg.ToolCallID] = run
				}
				out = append(out, message{ID: id, Run: run})
			}
			run.Output, run.Status = messageText(msg), "Done"
			if entry.Truncated {
				run.OutputFull = src.link(entry.EntryID, "", "output", nil)
			}
			if msg.IsError {
				run.Status = "Failed"
			}
		case "custom":
			if msg.CustomType != sdk.BashExecutionCustomType {
				continue
			}
			details, err := json.Marshal(msg.Details)
			if err != nil {
				continue
			}
			var bash sdk.BashExecution
			if json.Unmarshal(details, &bash) != nil || bash.Command == "" {
				continue
			}
			run := &execution{Name: "Shell", Command: bash.Command, Output: bash.Output, Truncated: bash.Truncated, Status: "Done"}
			if entry.Truncated {
				run.OutputFull = src.link(entry.EntryID, "", "output", nil)
			}
			if bash.Cancelled {
				run.Status = "Canceled"
			} else if bash.ExitCode != 0 {
				run.Status = fmt.Sprintf("Exit %d", bash.ExitCode)
			}
			out = append(out, message{ID: id, Run: run})
		}
		if msg.Role == "assistant" && msg.ErrorMessage != "" && messageText(msg) == "" {
			out = append(out, message{ID: id + ":error", Role: "error", Text: msg.ErrorMessage})
		}
	}
	for _, tool := range live.Tools {
		run := calls[tool.ID]
		if run == nil {
			run = executionInput(tool.Name, json.RawMessage(tool.Input))
			out = append(out, message{ID: "tool:" + tool.ID, Run: run, Live: true})
		}
		if run.Status == "" {
			run.Output, run.Status = tool.Progress, "Running"
			if tool.Truncated || run.Truncated {
				run.OutputFull = src.link(tool.ID, "tool", "output", nil)
			}
		}
	}
	for _, bash := range live.Bash {
		run := &execution{Name: "Shell", Command: bash.Input, Output: bash.Text, Status: "Running"}
		if bash.Truncated {
			run.OutputFull = src.link(bash.ID, "bash", "output", nil)
		}
		out = append(out, message{ID: "bash:" + bash.ID, Live: true, Run: run})
	}
	for _, msg := range live.Messages {
		if msg.Role == "assistant" && msg.Text != "" {
			bubble := message{ID: "live:" + msg.ID, Role: msg.Role, Text: msg.Text, Live: true}
			if msg.Truncated {
				bubble.Full = src.link(msg.ID, "message", "", nil)
			}
			out = append(out, bubble)
		}
	}
	return out
}
