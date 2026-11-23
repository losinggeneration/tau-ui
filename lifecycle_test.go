package main

import (
	"testing"

	"github.com/losinggeneration/tau/rpc/coord"
)

func TestResumeTarget(t *testing.T) {
	items := []coord.Instance{
		{ID: "current", CWD: "/a", SessionID: "open-here", LinkState: coord.LinkConnected},
		{ID: "holder", CWD: "/b", SessionID: "held", LinkState: coord.LinkConnected},
		{ID: "gone", CWD: "/a", SessionID: "exited", LinkState: coord.LinkExited},
	}
	for _, test := range []struct {
		name, id, project, target string
		launch                    bool
	}{
		{name: "held by another worker", id: "held", project: "/b", target: "holder"},
		{name: "already current", id: "open-here", project: "/a", target: "current"},
		{name: "saved in this project", id: "saved", project: "/a"},
		{name: "saved in another project", id: "saved", project: "/c", launch: true},
		{name: "exited worker does not hold it", id: "exited", project: "/a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, launch := resumeTarget(items, "current", test.id, test.project)
			if target != test.target || launch != test.launch {
				t.Fatalf("resumeTarget = %q, %t", target, launch)
			}
		})
	}
}
