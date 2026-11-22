package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/sdk"
)

type entryRequest struct {
	Session string `json:"session_id"`
	Entry   string `json:"entry_id"`
	Live    string `json:"live_kind,omitempty"`
	Image   *int   `json:"image_index,omitempty"`
	Offset  int    `json:"offset"`
}

func readEntry(ctx context.Context, c *client.CoordinatedClient, worker string, args entryRequest) ([]byte, error) {
	var out []byte
	for {
		args.Offset = len(out)
		reply, err := workerCommand(ctx, c, worker, "get_entry_content", args)
		if err != nil {
			return nil, err
		}
		encoded, dataOK := reply.Result["data"].(string)
		more, moreOK := reply.Result["more"].(bool)
		chunk, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || !dataOK || !moreOK || (more && len(chunk) == 0) {
			return nil, errors.New("Tau returned an invalid content chunk.")
		}
		out = append(out, chunk...)
		if !more {
			return out, nil
		}
	}
}

func rasterMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

type expandedContent struct {
	Message           message
	Run               *execution
	Output            string
	IsOutput, Limited bool
}

func (a app) entryContent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	part := q.Get("part")
	args := entryRequest{Session: q.Get("session"), Entry: q.Get("entry"), Live: q.Get("live")}
	var index int
	if q.Has("index") {
		var err error
		index, err = strconv.Atoi(q.Get("index"))
		if err != nil || index < 0 {
			http.Error(w, "Invalid content index", http.StatusBadRequest)
			return
		}
	}
	if args.Session == "" || args.Entry == "" || len(args.Session) > 256 || len(args.Entry) > 256 {
		http.Error(w, "Missing message or session", http.StatusBadRequest)
		return
	}
	if part == "image" {
		args.Image = &index
	}
	var data []byte
	worker := r.PathValue("id")
	err := a.rpc(r, 12*time.Second, func(ctx context.Context, c *client.CoordinatedClient) error {
		var err error
		data, err = readEntry(ctx, c, worker, args)
		return err
	})
	if part == "image" {
		if err != nil {
			http.Error(w, "Image unavailable", http.StatusNotFound)
			return
		}
		image, err := base64.StdEncoding.DecodeString(string(data))
		mime := http.DetectContentType(image)
		if err != nil || !rasterMIME(mime) {
			http.Error(w, "Unsupported image", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", mime)
		if _, err := w.Write(image); err != nil {
			return // The browser disconnected during transfer.
		}
		return
	}
	var content expandedContent
	if err == nil {
		content, err = (contentSource{worker, args.Session}).expand(data, args, part, index)
	}
	if err != nil {
		// Keep the preview and retry button visible when expansion fails.
		render(w, "content-error", err.Error())
		return
	}
	render(w, "content-expanded", content)
}

func (src contentSource) expand(data []byte, args entryRequest, part string, index int) (expandedContent, error) {
	var view expandedContent
	if args.Live != "" {
		var item liveItem
		if err := json.Unmarshal(data, &item); err != nil {
			return view, fmt.Errorf("decode live message: %w", err)
		}
		view.Limited = item.Truncated
		switch args.Live {
		case "message":
			view.Message.Text = item.Text
		case "tool":
			view.Run = executionInput(item.Name, json.RawMessage(item.Input))
			view.Output, view.IsOutput = item.Progress, true
		case "bash":
			view.Run = &execution{Command: item.Input}
			view.Output, view.IsOutput = item.Text, true
		}
		return view, nil
	}
	var msg sdk.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return view, fmt.Errorf("decode saved message: %w", err)
	}
	switch part {
	case "call", "text":
		if index >= len(msg.Content) {
			return view, errors.New("This part of the message is no longer available.")
		}
		block := msg.Content[index]
		if part == "call" {
			view.Run = executionInput(block.Name, block.Arguments)
		} else {
			view.Message.Text = block.Text
		}
	case "output":
		view.IsOutput = true
		view.Output = messageText(msg)
		if msg.CustomType == sdk.BashExecutionCustomType {
			var bash sdk.BashExecution
			if err := decodeDetails(msg.Details, &bash); err != nil {
				return view, err
			}
			view.Output, view.Limited = bash.Output, bash.Truncated
		}
	default:
		if msg.Role == "user" {
			view.Message = src.userMessage(args.Entry, msg)
		} else {
			view.Message.Text = messageText(msg)
		}
	}
	return view, nil
}

func decodeDetails(details any, value any) error {
	data, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode execution details: %w", err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode execution details: %w", err)
	}
	return nil
}
