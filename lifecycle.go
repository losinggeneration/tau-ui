package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"time"

	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
)

const (
	// lifecycleTimeout bounds the whole request and must stay below the HTTP
	// server's WriteTimeout so the reply can still be written.
	lifecycleTimeout = 50 * time.Second
	// restartStopBudget covers the session-preserving stop and the wait for
	// exit; the saved-session launch gets whatever remains.
	restartStopBudget = 20 * time.Second
)

// restartSupported reports whether c's coordinator can stop a worker while
// keeping its session and then launch that saved session again.
func restartSupported(c *client.CoordinatedClient) bool {
	return slices.Contains(c.Welcome().Operations, coord.OperationSavedSessionLaunch)
}

// lifecycle stops or restarts one worker. No step is retried: a mutation
// whose outcome is unknown is reported, never resent.
func (a app) lifecycle(w http.ResponseWriter, r *http.Request) {
	if !form(w, r) {
		return
	}
	op, session := r.PostForm.Get("op"), r.PostForm.Get("session")
	if (op != "stop" && op != "restart") || session == "" {
		result(w, "", errors.New("Choose stop or restart for a connected session."))
		return
	}
	id := r.PathValue("id")
	// Once a stop is sent, finish the sequence even if the browser goes away,
	// so a restart isn't abandoned between stop and resume.
	detached := r.WithContext(context.WithoutCancel(r.Context()))
	var next *coord.Instance
	err := a.rpc(detached, lifecycleTimeout, func(ctx context.Context, c *client.CoordinatedClient) error {
		target, err := lifecycleTarget(ctx, c, id, session)
		if err != nil {
			return err
		}
		if op == "stop" {
			return stopWorker(ctx, c, target)
		}
		next, err = restartWorker(ctx, c, target)
		return err
	})
	if err != nil {
		result(w, "", err)
		return
	}
	if r.PostForm.Get("active") == id {
		location := "/"
		if op == "restart" {
			location = "/worker/" + url.PathEscape(next.ID)
		}
		w.Header().Set("HX-Redirect", location)
	} else {
		// Keep the current conversation (and its unsent attachments) in place.
		w.Header().Set("HX-Trigger-After-Settle", "instanceListRefresh")
	}
	w.WriteHeader(http.StatusOK)
}

func lifecycleTarget(ctx context.Context, c *client.CoordinatedClient, id, session string) (coord.Instance, error) {
	items, err := c.ListInstances(ctx)
	if err != nil {
		return coord.Instance{}, err
	}
	i := slices.IndexFunc(items, func(inst coord.Instance) bool { return inst.ID == id })
	if i < 0 || items[i].LinkState == coord.LinkExited {
		return coord.Instance{}, errors.New("This instance is no longer running. Nothing was sent.")
	}
	if items[i].SessionID != session {
		return coord.Instance{}, errors.New("This instance switched sessions. Open the active session first.")
	}
	return items[i], nil
}

func stopWorker(ctx context.Context, c *client.CoordinatedClient, target coord.Instance) error {
	if err := c.StopInstance(ctx, target.ID); err != nil {
		return err
	}
	if err := waitExit(ctx, c, target.ID); err != nil {
		return fmt.Errorf("Tau accepted the stop, but the worker hasn't exited yet (%v). Check the instance list.", err)
	}
	return nil
}

func restartWorker(ctx context.Context, c *client.CoordinatedClient, target coord.Instance) (*coord.Instance, error) {
	// Check that the daemon can resume the session before stopping anything.
	if !restartSupported(c) {
		return nil, errors.New("This Tau daemon can't restart instances. Restart the daemon with the current Tau version. Nothing was stopped.")
	}
	if target.SessionID == "" || !filepath.IsAbs(target.CWD) {
		return nil, errors.New("This instance has no saved session to resume. Nothing was stopped.")
	}
	stopCtx, cancel := context.WithTimeout(ctx, restartStopBudget)
	defer cancel()
	if err := c.StopInstanceForRestart(stopCtx, target.ID, target.SessionID); err != nil {
		if errors.Is(err, client.ErrOutcomeUnknown) {
			return nil, err // result explains; nothing is resumed or resent
		}
		return nil, fmt.Errorf("Tau didn't stop the worker: %v", err)
	}
	if err := waitExit(stopCtx, c, target.ID); err != nil {
		return nil, fmt.Errorf("Tau accepted the restart, but the worker hasn't exited yet (%v). Session %s was not resumed; check the instance list before opening it.", err, target.SessionID)
	}
	inst, err := c.StartNamedSavedSession(ctx, target.CWD, target.SessionID, target.Name)
	if errors.Is(err, client.ErrOutcomeUnknown) {
		// Don't wrap: result would replace this with the generic notice.
		return nil, fmt.Errorf("The worker stopped, but Tau may or may not have resumed session %s. Check the instance list before trying again. Nothing was resent.", target.SessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("The worker stopped, but Tau couldn't resume session %s: %v. Its work is saved; resume it from the Tau TUI.", target.SessionID, err)
	}
	return inst, nil
}

// resumeElsewhere opens a session the current worker can't take over and
// returns its instance. It returns empty when the current worker should switch.
func resumeElsewhere(ctx context.Context, c *client.CoordinatedClient, current, ref string) (string, error) {
	id, project, err := c.LocateSession(ctx, ref, "")
	if err != nil {
		return "", err
	}
	items, err := c.ListInstances(ctx)
	if err != nil {
		return "", err
	}
	target, launch := resumeTarget(items, current, id, project)
	if !launch {
		return target, nil
	}
	inst, err := c.StartSavedSession(ctx, project, id)
	if err != nil {
		return "", err
	}
	return inst.ID, nil
}

// resumeTarget picks the worker already running id. Without one, a worker
// in another project can't switch to it, so the session needs its own.
func resumeTarget(items []coord.Instance, current, id, project string) (target string, launch bool) {
	var currentCWD string
	for _, inst := range items {
		if inst.SessionID == id && inst.LinkState != coord.LinkExited {
			return inst.ID, false
		}
		if inst.ID == current {
			currentCWD = inst.CWD
		}
	}
	return "", currentCWD != project
}

// waitExit polls until id is gone or marked exited, like the coordinated TUI.
func waitExit(ctx context.Context, c *client.CoordinatedClient, id string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		items, err := c.ListInstances(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(items, func(inst coord.Instance) bool { return inst.ID == id && inst.LinkState != coord.LinkExited }) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
