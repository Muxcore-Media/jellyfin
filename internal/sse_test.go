package internal

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestPluginEventType(t *testing.T) {
	raw := map[string]any{"state": "playing"}
	if got := pluginEventType("progress", raw); got != "playback.progress" {
		t.Fatalf("event name: %q", got)
	}
	if got := pluginEventType("", map[string]any{"state": "stopped"}); got != "playback.stopped" {
		t.Fatalf("state: %q", got)
	}
}

func TestHandlePluginSSEStopped(t *testing.T) {
	var mu sync.Mutex
	var lastType string
	testPublishHook = func(_ context.Context, eventType, _ string, _ []byte) error {
		mu.Lock()
		lastType = eventType
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { testPublishHook = nil })

	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	m.handlePluginSSEData("stopped", `{"sessionId":"sess-9","state":"stopped"}`)
	mu.Lock()
	defer mu.Unlock()
	if lastType != "playback.stopped" {
		t.Fatalf("type %q", lastType)
	}
}

func TestHandlePluginSSEPlayingJSON(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s1", "itemId": "i1", "userId": "u1", "state": "playing", "positionTicks": 100000000,
	})
	if got := pluginEventType("playing", map[string]any{"state": "playing"}); got != "playback.started" {
		t.Fatalf("got %q", got)
	}
	_ = raw
}
