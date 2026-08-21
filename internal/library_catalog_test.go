package internal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPublishLibraryCatalogEvent(t *testing.T) {
	var published bool
	testPublishHook = func(_ context.Context, eventType, _ string, payload []byte) error {
		if eventType != "playback.library.item" {
			t.Fatalf("event: %s", eventType)
		}
		published = len(payload) > 0
		return nil
	}
	t.Cleanup(func() { testPublishHook = nil })

	m := NewModule(Config{ID: "jf1", GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	m.publishLibraryCatalogEvent(context.Background(), "added", map[string]any{
		"itemId": "abc", "itemType": "Movie",
	})
	if !published {
		t.Fatal("expected publish")
	}
}

func TestHandlePluginLibrarySSE(t *testing.T) {
	var action string
	testPublishHook = func(_ context.Context, eventType, _ string, payload []byte) error {
		if eventType != "playback.library.item" {
			return nil
		}
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			t.Fatal(err)
		}
		action = strings.ToLower(raw["action"].(string))
		return nil
	}
	t.Cleanup(func() { testPublishHook = nil })

	m := NewModule(Config{ID: "jf1", GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	m.handlePluginLibrarySSE("library.item.removed", `{"itemId":"gone","itemType":"Episode"}`)
	if action != "removed" {
		t.Fatalf("action: %q", action)
	}
}
