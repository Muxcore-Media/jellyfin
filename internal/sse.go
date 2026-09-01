package internal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	playbackevents "github.com/Muxcore-Media/contracts-playback/events"
)

func (m *Module) sseLoop() {
	backoff := time.Second
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}
		if !m.sseEnabled() || !m.configured() {
			select {
			case <-m.stopCh:
				return
			case <-time.After(time.Second):
			}
			continue
		}
		err := m.runPluginSSE()
		m.setSSEConnected(false)
		if err != nil {
			slog.Debug("jellyfin: plugin sse disconnected", "error", err)
		}
		select {
		case <-m.stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (m *Module) runPluginSSE() error {
	m.mu.RLock()
	base, apiKey := m.baseURL, m.apiKey
	m.mu.RUnlock()
	reqURL := base + "/api/sse/events"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", `MediaBrowser Token="`+apiKey+`"`)

	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("plugin sse not available (404)")
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("plugin sse status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	m.setSSEConnected(true)
	slog.Info("jellyfin: plugin sse connected")
	return readJellyfinSSEStream(resp.Body, func(eventName, data string) error {
		select {
		case <-m.stopCh:
			return io.EOF
		default:
		}
		m.handlePluginSSEData(eventName, data)
		return nil
	})
}

func readJellyfinSSEStream(r io.Reader, onEvent func(eventName, data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var eventName string
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(dataLines) > 0 {
				if err := onEvent(eventName, strings.Join(dataLines, "\n")); err != nil {
					return err
				}
			}
			eventName = ""
			dataLines = nil
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (m *Module) handlePluginSSEData(eventName, data string) {
	data = strings.TrimSpace(data)
	if data == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(eventName)) {
	case "ping", "hello", "server.stats":
		return
	case "library.item.added":
		m.handlePluginLibrarySSE(eventName, data)
		return
	case "library.item.removed":
		m.handlePluginLibrarySSE(eventName, data)
		return
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	eventType := pluginEventType(eventName, raw)
	if eventType == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if eventType == playbackevents.EventPlaybackStopped {
		sessionID := stringField(raw, "sessionId", "session_id", "SessionId")
		if sessionID == "" {
			return
		}
		m.mu.Lock()
		delete(m.sessionSeen, sessionID)
		m.mu.Unlock()
		m.publishPlayback(ctx, eventType, playbackEventPayload{SessionID: sessionID})
		return
	}
	m.syncSessionFromPlugin(ctx, eventType, raw)
}

func pluginEventType(eventName string, raw map[string]any) string {
	switch strings.ToLower(strings.TrimSpace(eventName)) {
	case "playing", "session.start":
		return playbackevents.EventPlaybackStarted
	case "progress", "paused":
		return playbackevents.EventPlaybackProgress
	case "stopped", "session.end":
		return playbackevents.EventPlaybackStopped
	}
	state := strings.ToLower(stringField(raw, "state", "State"))
	switch state {
	case "playing", "started", "start":
		return playbackevents.EventPlaybackStarted
	case "paused", "progress":
		return playbackevents.EventPlaybackProgress
	case "stopped", "stop", "idle":
		return playbackevents.EventPlaybackStopped
	}
	return ""
}

func (m *Module) syncSessionFromPlugin(ctx context.Context, eventType string, raw map[string]any) {
	sessionID := stringField(raw, "sessionId", "session_id", "SessionId")
	itemID := stringField(raw, "itemId", "item_id", "ItemId")
	sessions, err := m.listSessions(ctx)
	if err != nil {
		ev := playbackEventPayload{
			ItemID:          itemID,
			JellyfinItemID:  itemID,
			MuxcoreID:       m.muxcoreIDForJellyfin(itemID),
			UserID:          stringField(raw, "userId", "user_id"),
			SessionID:       sessionID,
			PositionSeconds: ticksToSeconds(int64Field(raw, "positionTicks", "position_ticks")),
		}
		m.trackAndPublish(ctx, eventType, sessionID, ev)
		return
	}
	for _, s := range sessions {
		if sessionID != "" && s.Id != sessionID {
			continue
		}
		if itemID != "" && s.NowPlayingItem != nil && s.NowPlayingItem.ID != itemID {
			continue
		}
		if s.NowPlayingItem == nil {
			continue
		}
		key := s.Id
		if key == "" {
			key = s.UserId + ":" + s.NowPlayingItem.ID
		}
		pos := int64(0)
		paused := false
		if s.PlayState != nil {
			pos = s.PlayState.PositionTicks
			paused = s.PlayState.IsPaused
		}
		ev := playbackEventPayload{
			ItemID:          s.NowPlayingItem.ID,
			JellyfinItemID:  s.NowPlayingItem.ID,
			MuxcoreID:       m.muxcoreIDForJellyfin(s.NowPlayingItem.ID),
			UserID:          s.UserId,
			UserName:        s.UserName,
			SessionID:       s.Id,
			PositionTicks:   pos,
			PositionSeconds: ticksToSeconds(pos),
			MediaPath:       s.NowPlayingItem.Path,
			Title:           s.NowPlayingItem.Name,
			MediaType:       s.NowPlayingItem.Type,
			IsPaused:        paused,
			Platform:        s.Client,
			Device:          s.DeviceName,
			Player:          firstNonEmpty(s.AppName, s.Client),
			IPAddress:       remoteIP(s.RemoteEndPoint),
		}
		if s.PlayState != nil {
			ev.IsTranscode = strings.EqualFold(s.PlayState.PlayMethod, "Transcode") || s.TranscodingInfo != nil
			ev.PlayMethod = strings.TrimSpace(s.PlayState.PlayMethod)
		}
		ev.StreamResolution = streamResolutionFromJFSession(s)
		m.trackAndPublish(ctx, eventType, key, ev)
		return
	}
}

func (m *Module) trackAndPublish(ctx context.Context, eventType, key string, ev playbackEventPayload) {
	if key == "" {
		key = ev.SessionID
	}
	switch eventType {
	case playbackevents.EventPlaybackStarted:
		m.mu.Lock()
		if m.sessionSeen[key] == "" {
			m.sessionSeen[key] = "playing"
		}
		m.mu.Unlock()
	case playbackevents.EventPlaybackProgress:
		state := "playing"
		if ev.IsPaused {
			state = "paused"
		}
		m.mu.Lock()
		m.sessionSeen[key] = state
		m.mu.Unlock()
	}
	m.publishPlayback(ctx, eventType, ev)
	if eventType != playbackevents.EventPlaybackStopped {
		m.applyPlaybackToUserdata(ctx, ev, false)
	}
}

func (m *Module) sseEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sseEnabledFlag
}

func (m *Module) setSSEConnected(v bool) {
	m.sseMu.Lock()
	m.sseConnected = v
	m.sseMu.Unlock()
}

func (m *Module) sseConnectedNow() bool {
	m.sseMu.RLock()
	defer m.sseMu.RUnlock()
	return m.sseConnected
}

func envSSEEnabled(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
