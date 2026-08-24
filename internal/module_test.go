package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	playbackevents "github.com/Muxcore-Media/contracts-playback/events"
	jellyfinv1 "github.com/Muxcore-Media/jellyfin/proto/jellyfinv1"
	playbackv1 "github.com/Muxcore-Media/playback-contract/proto/playbackv1"
)

func testModule(t *testing.T, baseURL, apiKey string) *Module {
	t.Helper()
	return NewModule(Config{
		ID:            "jellyfin-test",
		GRPCAddr:      "127.0.0.1:0",
		HTTPAddr:      "127.0.0.1:0",
		BaseURL:       baseURL,
		APIKey:        apiKey,
		DataDir:       t.TempDir(),
		WebhookSecret: "s3cret",
		ConflictMode:  conflictJellyfin,
	})
}

func TestModuleInfo(t *testing.T) {
	m := NewModule(Config{DataDir: t.TempDir()})
	info := m.Info()
	if info.ID != "jellyfin" {
		t.Fatalf("id: %s", info.ID)
	}
	if info.Version != moduleVersion {
		t.Fatalf("version: %s", info.Version)
	}
	foundSettings, foundUD := false, false
	for _, c := range info.Capabilities {
		if c == "settings" {
			foundSettings = true
		}
		if c == "userdata.sync" {
			foundUD = true
		}
	}
	if !foundSettings || !foundUD {
		t.Fatalf("capabilities: %v", info.Capabilities)
	}
}

func TestLifecycle(t *testing.T) {
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DataDir: t.TempDir()})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsDurable(t *testing.T) {
	dir := t.TempDir()
	m := NewModule(Config{DataDir: dir})
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.updateSetting("base_url", "http://jf:8096"); err != nil {
		t.Fatal(err)
	}
	if err := m.updateSetting("api_key", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := m.updateSetting("webhook_secret", "wh"); err != nil {
		t.Fatal(err)
	}
	defs := m.settingsDefs()
	if defs[0].Value != "http://jf:8096" {
		t.Fatalf("base: %s", defs[0].Value)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "http://jf:8096") {
		t.Fatalf("settings not persisted: %s", data)
	}
	m2 := NewModule(Config{DataDir: dir})
	if err := m2.loadDurable(); err != nil {
		t.Fatal(err)
	}
	if m2.baseURL != "http://jf:8096" || m2.apiKey != "secret" || m2.webhookSecret != "wh" {
		t.Fatalf("reload: %q %q %q", m2.baseURL, m2.apiKey, m2.webhookSecret)
	}
}

func TestHealthProbesSystemInfo(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info" {
			http.NotFound(w, r)
			return
		}
		hits++
		if !strings.Contains(r.Header.Get("Authorization"), "tok") {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "JF"})
	}))
	defer srv.Close()

	m := testModule(t, srv.URL, "tok")
	if err := m.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits: %d", hits)
	}
	m2 := testModule(t, "", "")
	if err := m2.Health(context.Background()); err == nil {
		t.Fatal("expected not configured")
	}
}

func TestRefreshLibraryHTTP(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	m := testModule(t, srv.URL, "tok")
	if _, err := m.RefreshLibrary(context.Background(), &jellyfinv1.RefreshLibraryRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RefreshLibrary(context.Background(), &jellyfinv1.RefreshLibraryRequest{ItemId: "abc"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "POST /Library/Refresh" || !strings.Contains(paths[1], "/Items/abc/Refresh") {
		t.Fatalf("paths: %v", paths)
	}
}

func TestPlayURLAndStatus(t *testing.T) {
	m := testModule(t, "http://jf:8096", "tok")
	resp, err := m.PlayURL(context.Background(), &jellyfinv1.PlayURLRequest{ItemId: "xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Url, "id=xyz") {
		t.Fatalf("url: %s", resp.Url)
	}
	st, err := m.Status(context.Background(), &jellyfinv1.StatusRequest{})
	if err != nil || !st.Configured || st.BaseUrl != "http://jf:8096" {
		t.Fatalf("status: %+v %v", st, err)
	}
}

func TestWebhookAuthAndPlaybackEvents(t *testing.T) {
	m := testModule(t, "http://jf:8096", "tok")
	var (
		mu   sync.Mutex
		got  []string
		raws [][]byte
	)
	prev := testPublishHook
	testPublishHook = func(_ context.Context, eventType, _ string, payload []byte) error {
		mu.Lock()
		got = append(got, eventType)
		raws = append(raws, append([]byte(nil), payload...))
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { testPublishHook = prev })

	body := []byte(`{"NotificationType":"PlaybackStart","ItemId":"item1","UserId":"u1","NotificationUsername":"alice","PlaybackPositionTicks":10000000,"RunTimeTicks":600000000,"Name":"Movie"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	m.handleWebhook(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without secret, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set(headerWebhookSecret, "s3cret")
	rr = httptest.NewRecorder()
	m.handleWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code: %d body=%s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	hasJF, hasPB := false, false
	for i, typ := range got {
		if typ == "jellyfin.playbackstart" {
			hasJF = true
		}
		if typ == playbackevents.EventPlaybackStarted {
			hasPB = true
			msg, err := playbackv1.UnmarshalSessionEvent(raws[i])
			if err != nil {
				t.Fatal(err)
			}
			if msg.GetItemId() != "item1" || msg.GetUserName() != "alice" || msg.GetPositionSeconds() != 1 {
				t.Fatalf("payload: %+v", msg)
			}
		}
	}
	if !hasJF || !hasPB {
		t.Fatalf("events: %v", got)
	}
}

func TestItemLinkCRUDAndSync(t *testing.T) {
	var mu sync.Mutex
	items := []jfItem{{
		ID: "jf1", Name: "Film", Type: "Movie", Path: "/media/Film.mkv",
		ProviderIds: map[string]string{"Imdb": "tt1", "Tmdb": "10"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/Items":
			_ = json.NewEncoder(w).Encode(jfItemsResponse{Items: items})
		case "/System/Info":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := testModule(t, srv.URL, "tok")
	up, err := m.UpsertItemLink(context.Background(), &jellyfinv1.UpsertItemLinkRequest{
		Link: &jellyfinv1.ItemLink{MuxcoreId: "mc1", Path: "/media/Film.mkv", ProviderIds: map[string]string{"Imdb": "tt1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Link.GetMuxcoreId() != "mc1" {
		t.Fatal(up.Link)
	}
	match, err := m.MatchItem(context.Background(), &jellyfinv1.MatchItemRequest{
		MuxcoreId: "mc1", Path: "/media/Film.mkv", ProviderIds: map[string]string{"Imdb": "tt1"},
	})
	if err != nil || !match.Matched || match.Link.GetJellyfinId() != "jf1" {
		t.Fatalf("match: %+v %v", match, err)
	}
	syncResp, err := m.SyncLibrary(context.Background(), &jellyfinv1.SyncLibraryRequest{Direction: "both"})
	if err != nil {
		t.Fatal(err)
	}
	if syncResp.Scanned != 1 || syncResp.Upserted < 1 {
		t.Fatalf("sync: %+v", syncResp)
	}
	list, err := m.ListItemLinks(context.Background(), &jellyfinv1.ListItemLinksRequest{})
	if err != nil || len(list.Links) == 0 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err := m.DeleteItemLink(context.Background(), &jellyfinv1.DeleteItemLinkRequest{MuxcoreId: "mc1"}); err != nil {
		t.Fatal(err)
	}
}

func TestDialSubscribeOrdering(t *testing.T) {
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DataDir: t.TempDir()})
	t.Setenv("MUXCORE_GRPC_ADDR", "")
	done := make(chan struct{})
	go func() {
		m.connectCoreAndSubscribe()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connectCoreAndSubscribe hung without addr")
	}

	m2 := NewModule(Config{DataDir: t.TempDir()})
	close(m2.stopCh)
	t.Setenv("MUXCORE_GRPC_ADDR", "127.0.0.1:1")
	t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "true")
	done2 := make(chan struct{})
	go func() {
		m2.connectCoreAndSubscribe()
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("connect did not honor stopCh")
	}
}
