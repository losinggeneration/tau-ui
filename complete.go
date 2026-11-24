package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/tui/autocomplete"
)

// pathChoice is one @path suggestion with the draft it would produce, so the
// browser only has to swap in Text and place the cursor.
type pathChoice struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Text        string `json:"text"`
	Cursor      int    `json:"cursor"`
}

// completePath asks the worker for files matching the @path token before the
// cursor. Only the cursor's line goes to Tau, which keeps long drafts under
// the RPC frame limit. The browser counts the cursor in UTF-16 units, so it's
// converted to bytes on the way in and back on the way out.
func (a app) completePath(w http.ResponseWriter, r *http.Request) {
	text := r.PostFormValue("text")
	units, _ := strconv.Atoi(r.PostFormValue("cursor"))
	cursor := byteOffset(text, units)
	start := strings.LastIndexByte(text[:cursor], '\n') + 1
	end := len(text)
	if i := strings.IndexByte(text[cursor:], '\n'); i >= 0 {
		end = cursor + i
	}
	line := text[start:end]

	var out struct {
		Prefix string
		Items  []autocomplete.Item
	}
	err := a.rpc(r, 5*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		return call(ctx, c, r.PathValue("id"), "complete_path", map[string]any{"text": line, "cursor": cursor - start}, &out)
	})
	if err != nil && !unsupported(err) {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	choices := []pathChoice{}
	for _, item := range out.Items {
		res := autocomplete.ApplyCompletion([]string{line}, 0, cursor-start, item, out.Prefix)
		draft := text[:start] + res.Lines[0] + text[end:]
		choices = append(choices, pathChoice{
			Label: item.Label, Description: item.Description,
			Text: draft, Cursor: utf16Len(draft[:start+res.CursorCol]),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(choices); err != nil {
		log.Printf("send path suggestions: %v", err)
	}
}

// byteOffset converts a UTF-16 offset into s to a byte offset, clamped to s.
func byteOffset(s string, units int) int {
	for i, r := range s {
		if units <= 0 {
			return i
		}
		units -= utf16.RuneLen(r)
	}
	return len(s)
}

func utf16Len(s string) (n int) {
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
