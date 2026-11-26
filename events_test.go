package main

import (
	"slices"
	"testing"
)

func TestEventHubCoalescesAndScopes(t *testing.T) {
	h := newEventHub()
	open, other, list := h.subscribe("w1"), h.subscribe("w2"), h.subscribe("")
	for range 3 {
		h.publish(eventConversation, "w1")
	}
	h.publish(eventInstances, "")
	if got := open.take(); !slices.Equal(sorted(got), []string{eventConversation, eventInstances}) {
		t.Errorf("w1 page got %v", got)
	}
	if got := other.take(); !slices.Equal(got, []string{eventInstances}) {
		t.Errorf("w2 page got %v", got)
	}
	if got := list.take(); !slices.Equal(got, []string{eventInstances}) {
		t.Errorf("list page got %v", got)
	}
	if got := open.take(); len(got) != 0 {
		t.Errorf("second take got %v", got)
	}
	h.unsubscribe(open)
	h.publish(eventConversation, "w1") // must not block or panic
	var nilHub *eventHub
	nilHub.publish(eventInstances, "")
}

func sorted(s []string) []string { slices.Sort(s); return s }
