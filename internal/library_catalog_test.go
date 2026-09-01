package internal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	playbackevents "github.com/Muxcore-Media/contracts-playback/events"
)

func TestPublishLibraryCatalogEvent(t *testing.T) {
	var published bool
	var payload map[string]any
	testPublishHook = func(_ context.Context, eventType, _ string, raw []byte) error {
		if eventType != playbackevents.EventPlaybackLibraryItem {
			t.Fatalf("event: %s", eventType)
		}
		published = len(raw) > 0
		_ = json.Unmarshal(raw, &payload)
		return nil
	}
	t.Cleanup(func() { testPublishHook = nil })

	m := NewModule(Config{ID: "jf1", GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	m.publishLibraryCatalogEvent(context.Background(), "added", map[string]any{
		"itemId": "abc", "itemType": "Movie", "library_name": "Movies",
	})
	if !published {
		t.Fatal("expected publish")
	}
	if payload["library_name"] != "Movies" {
		t.Fatalf("library_name: %#v", payload)
	}
}

func TestHandlePluginLibrarySSE(t *testing.T) {
	var action string
	testPublishHook = func(_ context.Context, eventType, _ string, payload []byte) error {
		if eventType != playbackevents.EventPlaybackLibraryItem {
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
