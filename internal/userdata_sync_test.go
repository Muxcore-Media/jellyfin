package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUserdataSyncPullFromJellyfin(t *testing.T) {
	var (
		mu      sync.Mutex
		putBody []byte
		putUser string
		jfHits  []string
	)
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		jfHits = append(jfHits, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_ = json.NewEncoder(w).Encode([]jfUser{{ID: "u1", Name: "alice"}})
		case strings.HasPrefix(r.URL.Path, "/Users/u1/Items"):
			filter := r.URL.Query().Get("Filters")
			items := []jfItemWithUserData{}
			switch filter {
			case "IsResumable":
				items = append(items, jfItemWithUserData{
					ID: "jf-movie-1", Name: "Film A", Type: "Movie", RunTimeTicks: 6000000000,
					UserData: &jfUserData{PlaybackPositionTicks: 1200000000, LastPlayedDate: "2026-08-01T12:00:00Z"},
				})
			case "IsPlayed":
				items = append(items, jfItemWithUserData{
					ID: "jf-movie-2", Name: "Film B", Type: "Movie", RunTimeTicks: 5000000000,
					UserData: &jfUserData{Played: true, PlayCount: 1, LastPlayedDate: "2026-08-02T12:00:00Z"},
				})
			case "IsFavorite":
				items = append(items, jfItemWithUserData{
					ID: "jf-movie-1", Name: "Film A", Type: "Movie", ProductionYear: 2024,
					UserData: &jfUserData{IsFavorite: true},
				})
			}
			_ = json.NewEncoder(w).Encode(jfItemsUserDataResponse{Items: items})
		default:
			http.NotFound(w, r)
		}
	}))
	defer jf.Close()

	ud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/userdata" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		putUser = r.URL.Query().Get("user_id")
		putBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(putBody)
	}))
	defer ud.Close()

	m := testModule(t, jf.URL, "tok")
	m.userdataSync = true
	m.userdataLocalURL = ud.URL
	_ = m.storeLink(&ItemLink{MuxcoreID: "mc1", JellyfinID: "jf-movie-1", MediaKind: "movie", Title: "Film A"})

	res, err := m.syncUserdataFromJellyfin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Users != 1 || res.Progress < 1 || res.Favorites < 1 {
		t.Fatalf("result: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if putUser != "alice" {
		t.Fatalf("user_id=%q", putUser)
	}
	var blob muxUserdataBlob
	if err := json.Unmarshal(putBody, &blob); err != nil {
		t.Fatal(err)
	}
	p, ok := blob.Progress["mc1"]
	if !ok || p.PositionSec != 120 {
		t.Fatalf("progress mc1: %+v ok=%v", p, ok)
	}
	if _, ok := blob.Favorites["mc1"]; !ok {
		t.Fatalf("favorites missing: %#v", blob.Favorites)
	}
	watched, ok := blob.Progress["jf:jf-movie-2"]
	if !ok || !watched.Watched {
		t.Fatalf("watched: %+v", watched)
	}
}

func TestUserdataPushToJellyfin(t *testing.T) {
	var (
		mu    sync.Mutex
		posts []string
	)
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts = append(posts, r.Method+" "+r.URL.Path)
		if r.Body != nil {
			_, _ = io.ReadAll(r.Body)
		}
		mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_ = json.NewEncoder(w).Encode([]jfUser{{ID: "u1", Name: "alice"}})
		case strings.Contains(r.URL.Path, "/UserData") || strings.Contains(r.URL.Path, "/FavoriteItems"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jf.Close()

	m := testModule(t, jf.URL, "tok")
	m.userdataPushToJF = true
	_ = m.storeLink(&ItemLink{MuxcoreID: "mc1", JellyfinID: "jf1"})

	blob := muxUserdataBlob{
		Progress: map[string]muxProgressEntry{
			"mc1": {ID: "mc1", PositionSec: 42, DurationSec: 100, UpdatedAt: time.Now().UTC().Format(time.RFC3339)},
		},
		Favorites: map[string]muxFavoriteEntry{
			"mc1": {ID: "mc1", Title: "Film", Href: "/movies/mc1"},
		},
	}
	res, err := m.pushMuxUserdataToJellyfin(context.Background(), "alice", blob)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed < 2 {
		t.Fatalf("pushed: %+v posts=%v", res, posts)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(posts, "|")
	if !strings.Contains(joined, "POST /Users/u1/Items/jf1/UserData") {
		t.Fatalf("missing userdata post: %v", posts)
	}
	if !strings.Contains(joined, "POST /Users/u1/FavoriteItems/jf1") {
		t.Fatalf("missing favorite post: %v", posts)
	}
}

func TestUserdataHTTPEndpoints(t *testing.T) {
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users" {
			_ = json.NewEncoder(w).Encode([]jfUser{})
			return
		}
		http.NotFound(w, r)
	}))
	defer jf.Close()

	m := testModule(t, jf.URL, "tok")
	m.userdataSync = true
	m.userdataPushToJF = true

	req := httptest.NewRequest(http.MethodGet, "/userdata/status", nil)
	rr := httptest.NewRecorder()
	m.handleUserdataStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code %d", rr.Code)
	}
	var st map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if st["userdata_sync"] != true {
		t.Fatalf("status: %#v", st)
	}

	req = httptest.NewRequest(http.MethodPost, "/userdata/sync", nil)
	rr = httptest.NewRecorder()
	m.handleUserdataSync(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync code %d %s", rr.Code, rr.Body.String())
	}
}

func TestJellyfinURLEnvAlias(t *testing.T) {
	t.Setenv("JELLYFIN_BASE_URL", "")
	t.Setenv("JELLYFIN_URL", "http://jf-alias:8096")
	t.Setenv("USERDATA_SYNC", "1")
	t.Setenv("USERDATA_SYNC_INTERVAL_SECONDS", "120")
	t.Setenv("USERDATA_LOCAL_URL", "http://ud:9680")
	m := NewModule(Config{DataDir: t.TempDir()})
	if m.baseURL != "http://jf-alias:8096" {
		t.Fatalf("baseURL=%q", m.baseURL)
	}
	if !m.userdataSync || m.userdataSyncSec != 120 || m.userdataLocalURL != "http://ud:9680" {
		t.Fatalf("userdata cfg: sync=%v sec=%d url=%q", m.userdataSync, m.userdataSyncSec, m.userdataLocalURL)
	}
}

func TestPlaybackMirrorsUserdataWhenEnabled(t *testing.T) {
	var (
		mu      sync.Mutex
		putBody []byte
	)
	ud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		putBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ud.Close()

	m := testModule(t, "http://jf", "tok")
	m.userdataSync = true
	m.userdataLocalURL = ud.URL
	_ = m.storeLink(&ItemLink{MuxcoreID: "mc9", JellyfinID: "item9"})

	m.applyPlaybackToUserdata(context.Background(), playbackEventPayload{
		ItemID: "item9", JellyfinItemID: "item9", MuxcoreID: "mc9",
		UserID: "u1", UserName: "alice", PositionSeconds: 30, DurationSeconds: 100, Title: "T",
	}, false)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := len(putBody) > 0
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var blob muxUserdataBlob
	if err := json.Unmarshal(putBody, &blob); err != nil {
		t.Fatalf("body=%q err=%v", putBody, err)
	}
	p := blob.Progress["mc9"]
	if p.PositionSec != 30 {
		t.Fatalf("progress: %+v", p)
	}
}
