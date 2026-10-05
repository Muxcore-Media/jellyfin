package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MuxCore userdata wire shapes (aligned with media-ui-app + userdata-local/store).
type muxProgressEntry struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	PosterURL   string `json:"poster_url,omitempty"`
	Href        string `json:"href"`
	StreamURL   string `json:"stream_url,omitempty"`
	PositionSec int64  `json:"positionSec"`
	DurationSec int64  `json:"durationSec"`
	UpdatedAt   string `json:"updatedAt"`
	Watched     bool   `json:"watched,omitempty"`
}

type muxFavoriteEntry struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	PosterURL string `json:"poster_url,omitempty"`
	Href      string `json:"href"`
	Year      int    `json:"year,omitempty"`
}

type muxUserdataBlob struct {
	Progress  map[string]muxProgressEntry `json:"progress"`
	Favorites map[string]muxFavoriteEntry `json:"favorites"`
	UpdatedAt string                      `json:"updated_at,omitempty"`
	UserID    string                      `json:"user_id,omitempty"`
	TenantID  string                      `json:"tenant_id,omitempty"`
}

type jfUser struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

type jfUserData struct {
	PlaybackPositionTicks int64  `json:"PlaybackPositionTicks"`
	PlayCount             int    `json:"PlayCount"`
	IsFavorite            bool   `json:"IsFavorite"`
	Played                bool   `json:"Played"`
	LastPlayedDate        string `json:"LastPlayedDate"`
}

type jfItemWithUserData struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	Type              string            `json:"Type"`
	Path              string            `json:"Path"`
	ProductionYear    int               `json:"ProductionYear"`
	RunTimeTicks      int64             `json:"RunTimeTicks"`
	ProviderIds       map[string]string `json:"ProviderIds"`
	UserData          *jfUserData       `json:"UserData"`
	SeriesName        string            `json:"SeriesName"`
	IndexNumber       int               `json:"IndexNumber"`
	ParentIndexNumber int               `json:"ParentIndexNumber"`
}

type jfItemsUserDataResponse struct {
	Items []jfItemWithUserData `json:"Items"`
}

type userdataSyncResult struct {
	Users     int      `json:"users"`
	Progress  int      `json:"progress"`
	Favorites int      `json:"favorites"`
	Pushed    int      `json:"pushed,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseUserMap(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func (m *Module) userdataSyncEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.userdataSync
}

func (m *Module) pollUserdataLoop() {
	for {
		m.mu.RLock()
		enabled := m.userdataSync
		sec := m.userdataSyncSec
		m.mu.RUnlock()
		wait := time.Second
		if enabled {
			if sec <= 0 {
				sec = 300
			}
			wait = time.Duration(sec) * time.Second
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if _, err := m.syncUserdataFromJellyfin(ctx); err != nil {
				slog.Debug("jellyfin: userdata sync failed", "error", err)
			}
			cancel()
		}
		select {
		case <-m.stopCh:
			return
		case <-time.After(wait):
		}
	}
}

// syncUserdataFromJellyfin pulls JF UserData (progress/favorites/watched) into MuxCore userdata.
func (m *Module) syncUserdataFromJellyfin(ctx context.Context) (*userdataSyncResult, error) {
	if !m.configured() {
		return &userdataSyncResult{Errors: []string{"jellyfin not configured"}}, nil
	}
	users, err := m.listJellyfinUsers(ctx)
	if err != nil {
		return nil, err
	}
	res := &userdataSyncResult{}
	for _, u := range users {
		muxUser := m.mapJellyfinUser(u)
		blob, nProg, nFav, err := m.buildMuxBlobFromJellyfinUser(ctx, u)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("user %s: %v", u.Name, err))
			continue
		}
		res.Users++
		res.Progress += nProg
		res.Favorites += nFav
		if err := m.writeMuxUserdata(ctx, muxUser, blob); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("write %s: %v", muxUser, err))
		}
	}
	return res, nil
}

func (m *Module) mapJellyfinUser(u jfUser) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mapped, ok := m.userdataUserMap[u.ID]; ok && mapped != "" {
		return mapped
	}
	if mapped, ok := m.userdataUserMap[u.Name]; ok && mapped != "" {
		return mapped
	}
	if u.Name != "" {
		return u.Name
	}
	return u.ID
}

func (m *Module) buildMuxBlobFromJellyfinUser(ctx context.Context, u jfUser) (muxUserdataBlob, int, int, error) {
	blob := muxUserdataBlob{
		Progress:  map[string]muxProgressEntry{},
		Favorites: map[string]muxFavoriteEntry{},
		UserID:    m.mapJellyfinUser(u),
	}
	// Resumable + played cover progress/watched; favorites separately.
	for _, filter := range []string{"IsResumable", "IsPlayed", "IsFavorite"} {
		items, err := m.listJellyfinUserItems(ctx, u.ID, filter)
		if err != nil {
			return blob, 0, 0, err
		}
		for _, it := range items {
			m.mergeJFItemIntoBlob(&blob, it)
		}
	}
	return blob, len(blob.Progress), len(blob.Favorites), nil
}

func (m *Module) mergeJFItemIntoBlob(blob *muxUserdataBlob, it jfItemWithUserData) {
	if it.ID == "" {
		return
	}
	muxID := m.muxcoreIDForJellyfin(it.ID)
	if muxID == "" {
		muxID = "jf:" + it.ID
	}
	kind := mediaKindFromJF(it.Type)
	href := muxHref(kind, muxID)
	title := it.Name
	if it.Type == "Episode" && it.SeriesName != "" {
		title = fmt.Sprintf("%s S%02dE%02d", it.SeriesName, it.ParentIndexNumber, it.IndexNumber)
		if it.Name != "" {
			title += " — " + it.Name
		}
	}
	ud := it.UserData
	if ud == nil {
		ud = &jfUserData{}
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	if ud.LastPlayedDate != "" {
		if t, err := time.Parse(time.RFC3339Nano, ud.LastPlayedDate); err == nil {
			updatedAt = t.UTC().Format(time.RFC3339)
		} else if t, err := time.Parse(time.RFC3339, ud.LastPlayedDate); err == nil {
			updatedAt = t.UTC().Format(time.RFC3339)
		}
	}
	if ud.IsFavorite {
		blob.Favorites[muxID] = muxFavoriteEntry{
			ID:    muxID,
			Kind:  kind,
			Title: title,
			Href:  href,
			Year:  it.ProductionYear,
		}
	}
	watched := ud.Played
	posSec := ticksToSeconds(ud.PlaybackPositionTicks)
	durSec := ticksToSeconds(it.RunTimeTicks)
	if watched || posSec > 0 || ud.PlayCount > 0 {
		entry := muxProgressEntry{
			ID:          muxID,
			Kind:        kind,
			Title:       title,
			Href:        href,
			PositionSec: posSec,
			DurationSec: durSec,
			UpdatedAt:   updatedAt,
			Watched:     watched,
		}
		if watched {
			entry.PositionSec = 0
		}
		if cur, ok := blob.Progress[muxID]; ok {
			if entry.UpdatedAt < cur.UpdatedAt {
				return
			}
		}
		blob.Progress[muxID] = entry
	}
}

func muxHref(kind, id string) string {
	switch kind {
	case "movie":
		return "/movies/" + id
	case "tv", "episode":
		return "/tv/" + id
	default:
		return "/library/" + id
	}
}

func (m *Module) listJellyfinUsers(ctx context.Context) ([]jfUser, error) {
	body, code, err := m.jellyfinGET(ctx, "/Users")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("jellyfin /Users status %d", code)
	}
	var users []jfUser
	if err := json.Unmarshal(body, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (m *Module) listJellyfinUserItems(ctx context.Context, userID, filter string) ([]jfItemWithUserData, error) {
	var all []jfItemWithUserData
	basePath := fmt.Sprintf("/Users/%s/Items", url.PathEscape(userID))
	for start := 0; ; start += jfItemsPageSize {
		q := url.Values{}
		q.Set("Recursive", "true")
		q.Set("IncludeItemTypes", jfIncludeItemTypes)
		q.Set("Fields", "Path,ProviderIds,UserData,SeriesName")
		q.Set("EnableTotalRecordCount", "false")
		q.Set("Filters", filter)
		q.Set("StartIndex", strconv.Itoa(start))
		q.Set("Limit", strconv.Itoa(jfItemsPageSize))
		path := basePath + "?" + q.Encode()
		body, code, err := m.jellyfinGET(ctx, path)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("jellyfin %s status %d", path, code)
		}
		var raw jfItemsUserDataResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		all = append(all, raw.Items...)
		if len(raw.Items) < jfItemsPageSize {
			break
		}
	}
	return all, nil
}

func (m *Module) writeMuxUserdata(ctx context.Context, userID string, blob muxUserdataBlob) error {
	blob.UserID = userID
	if blob.Progress == nil {
		blob.Progress = map[string]muxProgressEntry{}
	}
	if blob.Favorites == nil {
		blob.Favorites = map[string]muxFavoriteEntry{}
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return err
	}
	m.mu.RLock()
	base := m.userdataLocalURL
	m.mu.RUnlock()
	if base != "" {
		return m.putUserdataLocal(ctx, base, userID, data)
	}
	// No mesh store — publish so other consumers can apply.
	return m.publishEvent(ctx, "userdata.jellyfin.synced", data)
}

func (m *Module) putUserdataLocal(ctx context.Context, base, userID string, body []byte) error {
	rawURL := strings.TrimRight(base, "/") + "/userdata"
	if err := guardOutboundURL(rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Set("user_id", userID)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)
	resp, err := m.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("userdata-local PUT status %d", resp.StatusCode)
	}
	return nil
}

// pushMuxUserdataToJellyfin writes companion MuxCore progress/favorites into Jellyfin UserData.
func (m *Module) pushMuxUserdataToJellyfin(ctx context.Context, muxUser string, blob muxUserdataBlob) (*userdataSyncResult, error) {
	if !m.configured() {
		return &userdataSyncResult{Errors: []string{"jellyfin not configured"}}, nil
	}
	users, err := m.listJellyfinUsers(ctx)
	if err != nil {
		return nil, err
	}
	var jfUID string
	for _, u := range users {
		if m.mapJellyfinUser(u) == muxUser || u.Name == muxUser || u.ID == muxUser {
			jfUID = u.ID
			break
		}
	}
	if jfUID == "" {
		return &userdataSyncResult{Errors: []string{fmt.Sprintf("no jellyfin user for %q", muxUser)}}, nil
	}
	res := &userdataSyncResult{Users: 1}
	for id, p := range blob.Progress {
		jfID := m.jellyfinIDForMuxcore(id)
		if jfID == "" {
			if strings.HasPrefix(id, "jf:") {
				jfID = strings.TrimPrefix(id, "jf:")
			} else {
				continue
			}
		}
		if err := m.setJellyfinUserData(ctx, jfUID, jfID, p.PositionSec, p.DurationSec, p.Watched, false, false); err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Pushed++
		res.Progress++
	}
	for id := range blob.Favorites {
		jfID := m.jellyfinIDForMuxcore(id)
		if jfID == "" {
			if strings.HasPrefix(id, "jf:") {
				jfID = strings.TrimPrefix(id, "jf:")
			} else {
				continue
			}
		}
		if err := m.setJellyfinFavorite(ctx, jfUID, jfID, true); err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Pushed++
		res.Favorites++
	}
	return res, nil
}

func (m *Module) jellyfinIDForMuxcore(muxID string) string {
	if muxID == "" {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if link, ok := m.links[muxID]; ok && link != nil {
		return link.JellyfinID
	}
	for _, link := range m.links {
		if link != nil && link.MuxcoreID == muxID {
			return link.JellyfinID
		}
	}
	return ""
}

func (m *Module) setJellyfinFavorite(ctx context.Context, userID, itemID string, fav bool) error {
	path := fmt.Sprintf("/Users/%s/FavoriteItems/%s", url.PathEscape(userID), url.PathEscape(itemID))
	method := http.MethodPost
	if !fav {
		method = http.MethodDelete
	}
	return m.jellyfinRequest(ctx, method, path, nil)
}

func (m *Module) setJellyfinUserData(ctx context.Context, userID, itemID string, posSec, durSec int64, played, setPlayed, setFav bool) error {
	_ = durSec
	_ = setPlayed
	_ = setFav
	ticks := posSec * 10_000_000
	body := map[string]any{
		"PlaybackPositionTicks": ticks,
		"Played":                played,
	}
	if played {
		body["PlaybackPositionTicks"] = 0
	}
	path := fmt.Sprintf("/Users/%s/Items/%s/UserData", url.PathEscape(userID), url.PathEscape(itemID))
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return m.jellyfinRequest(ctx, http.MethodPost, path, raw)
}

func (m *Module) jellyfinRequest(ctx context.Context, method, path string, body []byte) error {
	m.mu.RLock()
	base, key := m.baseURL, m.apiKey
	m.mu.RUnlock()
	if base == "" || key == "" {
		return fmt.Errorf("jellyfin not configured")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	if err := guardOutboundURL(base + path); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, key))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.httpCli.Do(req)
	if err != nil {
		return err
	}
	discardBody(resp)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin %s %s status %d", method, path, resp.StatusCode)
	}
	return nil
}

// applyPlaybackToUserdata mirrors a live playback event into MuxCore userdata (when sync enabled).
func (m *Module) applyPlaybackToUserdata(ctx context.Context, ev playbackEventPayload, stopped bool) {
	if !m.userdataSyncEnabled() {
		return
	}
	if ev.ItemID == "" && ev.JellyfinItemID == "" {
		return
	}
	jfID := ev.JellyfinItemID
	if jfID == "" {
		jfID = ev.ItemID
	}
	muxID := ev.MuxcoreID
	if muxID == "" {
		muxID = m.muxcoreIDForJellyfin(jfID)
	}
	if muxID == "" {
		muxID = "jf:" + jfID
	}
	userID := ev.UserName
	if userID == "" {
		userID = ev.UserID
	}
	if userID == "" {
		userID = "anonymous"
	}
	// Remap JF user id → mux username when map / Users list known.
	if ev.UserID != "" {
		userID = m.mapJellyfinUser(jfUser{ID: ev.UserID, Name: ev.UserName})
	}
	watched := stopped && ev.DurationSeconds > 0 &&
		float64(ev.PositionSeconds)/float64(ev.DurationSeconds) >= 0.92
	if stopped && ev.PositionSeconds == 0 && ev.DurationSeconds > 0 {
		watched = true
	}
	kind := mediaKindFromJF(ev.MediaType)
	entry := muxProgressEntry{
		ID:          muxID,
		Kind:        kind,
		Title:       firstNonEmpty(ev.Title, muxID),
		Href:        muxHref(kind, muxID),
		PositionSec: ev.PositionSeconds,
		DurationSec: ev.DurationSeconds,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		Watched:     watched,
	}
	if watched {
		entry.PositionSec = 0
	}
	blob := muxUserdataBlob{
		Progress:  map[string]muxProgressEntry{muxID: entry},
		Favorites: map[string]muxFavoriteEntry{},
		UserID:    userID,
	}
	if err := m.writeMuxUserdata(ctx, userID, blob); err != nil {
		slog.Debug("jellyfin: userdata mirror failed", "error", err)
	}
}

func (m *Module) handleUserdataSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.checkWebhookAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := m.syncUserdataFromJellyfin(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (m *Module) handleUserdataFromMuxcore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// ADR-0019 / NFR-SEC-007: the end user comes from the bearer token, never
	// from X-User-ID. Header-only identity is a gated legacy path.
	var tokenUser string
	legacy := false
	if bearerToken(r) != "" {
		id, status, msg := m.authenticateUser(r)
		if id == nil {
			http.Error(w, msg, status)
			return
		}
		tokenUser = id.ID
	} else if legacyHeaderTrusted() {
		if !m.checkWebhookAuth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		legacy = true
		slog.Warn("jellyfin: trusting caller-supplied user id header (legacy; set a bearer token instead)", "env", envTrustCallerHeader)
	} else {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	m.mu.RLock()
	push := m.userdataPushToJF
	m.mu.RUnlock()
	if !push {
		http.Error(w, `{"error":"userdata push to jellyfin disabled; set USERDATA_PUSH_TO_JELLYFIN=1"}`, http.StatusServiceUnavailable)
		return
	}
	var blob muxUserdataBlob
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&blob); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	claimed := []string{
		strings.TrimSpace(r.URL.Query().Get("user_id")),
		strings.TrimSpace(r.Header.Get("X-User-ID")),
		strings.TrimSpace(blob.UserID),
	}
	userID := tokenUser
	if legacy {
		userID = ""
	}
	for _, c := range claimed {
		if c == "" {
			continue
		}
		if !legacy && c != tokenUser {
			http.Error(w, `{"error":"user id does not match authenticated principal"}`, http.StatusForbidden)
			return
		}
		if userID == "" {
			userID = c
		}
	}
	if userID == "" {
		http.Error(w, `{"error":"user_id required"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := m.pushMuxUserdataToJellyfin(ctx, userID, blob)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (m *Module) handleUserdataStatus(w http.ResponseWriter, r *http.Request) {
	if !m.checkWebhookAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"userdata_sync":              m.userdataSync,
		"userdata_sync_interval_sec": m.userdataSyncSec,
		"userdata_local_url":         m.userdataLocalURL != "",
		"userdata_push_to_jellyfin":  m.userdataPushToJF,
		"configured":                 m.baseURL != "" && m.apiKey != "",
	})
}

func parseSyncInterval(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
