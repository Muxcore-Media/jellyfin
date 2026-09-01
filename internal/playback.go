package internal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	playbackevents "github.com/Muxcore-Media/contracts-playback/events"
	playbackv1 "github.com/Muxcore-Media/playback-contract/proto/playbackv1"
)

type playbackEventPayload struct {
	ItemID           string `json:"item_id"`
	JellyfinItemID   string `json:"jellyfin_item_id"`
	MuxcoreID        string `json:"muxcore_id,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	UserName         string `json:"user_name,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	PositionTicks    int64  `json:"position_ticks"`
	DurationTicks    int64  `json:"duration_ticks"`
	PositionSeconds  int64  `json:"position_seconds"`
	DurationSeconds  int64  `json:"duration_seconds"`
	MediaPath        string `json:"media_path,omitempty"`
	Title            string `json:"title,omitempty"`
	MediaType        string `json:"media_type,omitempty"`
	IsPaused         bool   `json:"is_paused,omitempty"`
	IsTranscode      bool   `json:"is_transcode,omitempty"`
	PlayMethod       string `json:"play_method,omitempty"`
	Platform         string `json:"platform,omitempty"`
	Device           string `json:"device,omitempty"`
	Player           string `json:"player,omitempty"`
	IPAddress        string `json:"ip_address,omitempty"`
	NotificationType string `json:"notification_type,omitempty"`
	StreamResolution string `json:"stream_resolution,omitempty"`
	VideoHeight      int    `json:"video_height,omitempty"`
	VideoWidth       int    `json:"video_width,omitempty"`
}

func (m *Module) checkWebhookAuth(r *http.Request) bool {
	m.mu.RLock()
	secret := m.webhookSecret
	m.mu.RUnlock()
	if secret == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get(headerWebhookSecret))
	if got == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

func (m *Module) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if !m.checkWebhookAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	notif, _ := payload["NotificationType"].(string)
	event := "jellyfin.library.changed"
	if notif != "" {
		event = "jellyfin." + strings.ToLower(notif)
	}
	_ = m.publishEvent(r.Context(), event, body)
	m.publishPlaybackFromWebhook(r.Context(), notif, payload)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (m *Module) publishPlaybackFromWebhook(ctx context.Context, notif string, payload map[string]any) {
	var eventType string
	switch strings.ToLower(notif) {
	case "playbackstart":
		eventType = playbackevents.EventPlaybackStarted
	case "playbackprogress":
		eventType = playbackevents.EventPlaybackProgress
	case "playbackstop":
		eventType = playbackevents.EventPlaybackStopped
	default:
		return
	}
	itemID := stringField(payload, "ItemId", "ItemID", "Id")
	if item, ok := payload["Item"].(map[string]any); ok {
		if itemID == "" {
			itemID = stringField(item, "Id", "ItemId")
		}
	}
	userID := stringField(payload, "UserId", "UserID")
	userName := stringField(payload, "NotificationUsername", "UserName", "Username")
	sessionID := stringField(payload, "SessionId", "SessionID", "Id")
	path := stringField(payload, "Path")
	title := stringField(payload, "Name", "Title")
	if item, ok := payload["Item"].(map[string]any); ok {
		if path == "" {
			path = stringField(item, "Path")
		}
		if title == "" {
			title = stringField(item, "Name")
		}
	}
	posTicks := int64Field(payload, "PlaybackPositionTicks", "PositionTicks")
	durTicks := int64Field(payload, "RunTimeTicks", "DurationTicks")
	if item, ok := payload["Item"].(map[string]any); ok && durTicks == 0 {
		durTicks = int64Field(item, "RunTimeTicks")
	}
	mediaType := ""
	if item, ok := payload["Item"].(map[string]any); ok {
		mediaType = stringField(item, "Type")
	}
	ev := playbackEventPayload{
		ItemID:           itemID,
		JellyfinItemID:   itemID,
		MuxcoreID:        m.muxcoreIDForJellyfin(itemID),
		UserID:           userID,
		UserName:         userName,
		SessionID:        sessionID,
		PositionTicks:    posTicks,
		DurationTicks:    durTicks,
		PositionSeconds:  ticksToSeconds(posTicks),
		DurationSeconds:  ticksToSeconds(durTicks),
		MediaPath:        path,
		Title:            title,
		MediaType:        mediaType,
		NotificationType: notif,
	}
	enrichPlaybackFromMap(&ev, payload)
	m.publishPlayback(ctx, eventType, ev)
	m.applyPlaybackToUserdata(ctx, ev, eventType == playbackevents.EventPlaybackStopped)
}

func (m *Module) publishPlayback(ctx context.Context, eventType string, ev playbackEventPayload) {
	data, err := playbackv1.MarshalSessionEvent(m.sessionInputFromPayload(eventType, ev))
	if err != nil {
		return
	}
	if err := m.publishEvent(ctx, eventType, data); err != nil {
		slog.Debug("jellyfin: publish playback failed", "type", eventType, "error", err)
	}
}

func (m *Module) sessionInputFromPayload(eventType string, ev playbackEventPayload) playbackv1.SessionInput {
	itemID := ev.ItemID
	if itemID == "" {
		itemID = ev.JellyfinItemID
	}
	return playbackv1.SessionInput{
		EventType:         eventType,
		SourceModule:      m.id,
		ServerID:          m.id,
		ServerType:        "jellyfin",
		ExternalSessionID: ev.SessionID,
		UserID:            ev.UserID,
		UserName:          ev.UserName,
		ItemID:            itemID,
		MuxcoreID:         ev.MuxcoreID,
		Title:             ev.Title,
		MediaType:         ev.MediaType,
		PositionSeconds:   ev.PositionSeconds,
		DurationSeconds:   ev.DurationSeconds,
		IsPaused:          ev.IsPaused,
		IsTranscode:       ev.IsTranscode,
		PlayMethod:        ev.PlayMethod,
		Platform:          ev.Platform,
		Device:            ev.Device,
		Player:            ev.Player,
		IPAddress:         ev.IPAddress,
		MediaPath:         ev.MediaPath,
		StreamResolution:  streamResolutionFromPayload(ev),
	}
}

func (m *Module) pollSessionsLoop() {
	for {
		m.mu.RLock()
		sec := m.sessionsPollSec
		m.mu.RUnlock()
		wait := time.Second
		if sec > 0 && m.configured() {
			if m.sseConnectedNow() {
				wait = time.Duration(sec*2) * time.Second
				if wait < 60*time.Second {
					wait = 60 * time.Second
				}
			} else {
				wait = time.Duration(sec) * time.Second
				m.pollSessionsOnce()
			}
		}
		select {
		case <-m.stopCh:
			return
		case <-time.After(wait):
		}
		if m.sseConnectedNow() {
			m.pollSessionsOnce()
		}
	}
}

func (m *Module) pollSessionsOnce() {
	if !m.configured() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessions, err := m.listSessions(ctx)
	if err != nil {
		slog.Debug("jellyfin: sessions poll failed", "error", err)
		return
	}
	active := map[string]bool{}
	for _, s := range sessions {
		if s.NowPlayingItem == nil {
			continue
		}
		key := s.Id
		if key == "" {
			key = s.UserId + ":" + s.NowPlayingItem.ID
		}
		active[key] = true
		pos := int64(0)
		paused := false
		if s.PlayState != nil {
			pos = s.PlayState.PositionTicks
			paused = s.PlayState.IsPaused
		}
		state := "playing"
		if paused {
			state = "paused"
		}
		m.mu.Lock()
		prev := m.sessionSeen[key]
		m.sessionSeen[key] = state
		m.mu.Unlock()
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
		switch prev {
		case "":
			m.publishPlayback(ctx, playbackevents.EventPlaybackStarted, ev)
			m.applyPlaybackToUserdata(ctx, ev, false)
		default:
			m.publishPlayback(ctx, playbackevents.EventPlaybackProgress, ev)
			m.applyPlaybackToUserdata(ctx, ev, false)
		}
	}
	m.mu.Lock()
	for key := range m.sessionSeen {
		if !active[key] {
			delete(m.sessionSeen, key)
			// stopped event without item details when session disappears
			m.mu.Unlock()
			m.publishPlayback(ctx, playbackevents.EventPlaybackStopped, playbackEventPayload{SessionID: key})
			m.mu.Lock()
		}
	}
	m.mu.Unlock()
}

func (m *Module) muxcoreIDForJellyfin(jfID string) string {
	if jfID == "" {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if link, ok := m.links["jf:"+jfID]; ok && link != nil {
		return link.MuxcoreID
	}
	for _, link := range m.links {
		if link != nil && link.JellyfinID == jfID {
			return link.MuxcoreID
		}
	}
	return ""
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func int64Field(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case int64:
				return t
			case json.Number:
				n, _ := t.Int64()
				return n
			}
		}
	}
	return 0
}

func ticksToSeconds(ticks int64) int64 {
	if ticks <= 0 {
		return 0
	}
	return ticks / 10_000_000
}

func enrichPlaybackFromMap(ev *playbackEventPayload, payload map[string]any) {
	if ev == nil {
		return
	}
	if session, ok := payload["Session"].(map[string]any); ok {
		if ev.Platform == "" {
			ev.Platform = stringField(session, "Client")
		}
		if ev.Device == "" {
			ev.Device = stringField(session, "DeviceName", "Device")
		}
		if ev.Player == "" {
			ev.Player = stringField(session, "AppName", "Client")
		}
		if ev.IPAddress == "" {
			ev.IPAddress = remoteIP(stringField(session, "RemoteEndPoint"))
		}
		if ps, ok := session["PlayState"].(map[string]any); ok {
			if ev.PlayMethod == "" {
				ev.PlayMethod = strings.TrimSpace(stringField(ps, "PlayMethod"))
			}
			if !ev.IsTranscode {
				ev.IsTranscode = strings.EqualFold(ev.PlayMethod, "Transcode")
			}
		}
		if _, ok := session["TranscodingInfo"]; ok {
			ev.IsTranscode = true
		}
	}
}

func remoteIP(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if i := strings.LastIndex(endpoint, ":"); i > 0 {
		host := endpoint[:i]
		if strings.HasPrefix(host, "[") && strings.Contains(host, "]") {
			return strings.Trim(host, "[]")
		}
		return host
	}
	return endpoint
}
