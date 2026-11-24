package main

import (
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
}
