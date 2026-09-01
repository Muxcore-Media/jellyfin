package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestListJellyfinItemsPaginated(t *testing.T) {
	const total = 150
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items" {
			http.NotFound(w, r)
			return
		}
		start := 0
		if v := r.URL.Query().Get("StartIndex"); v != "" {
			fmt.Sscanf(v, "%d", &start)
		}
		limit := jfItemsPageSize
		if v := r.URL.Query().Get("Limit"); v != "" {
			fmt.Sscanf(v, "%d", &limit)
		}
		mu.Lock()
		defer mu.Unlock()
		end := start + limit
		if end > total {
			end = total
		}
		items := make([]jfItem, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, jfItem{ID: fmt.Sprintf("item-%d", i), Name: fmt.Sprintf("Title %d", i), Type: "Movie"})
		}
		_ = json.NewEncoder(w).Encode(jfItemsResponse{Items: items})
	}))
	defer srv.Close()

	m := testModule(t, srv.URL, "tok")
	items, err := m.listJellyfinItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != total {
		t.Fatalf("got %d items, want %d", len(items), total)
	}
}

func TestMediaKindFromJFMusicAndBooks(t *testing.T) {
	cases := map[string]string{
		"Audio":      "music",
		"MusicAlbum": "music",
		"AudioBook":  "audiobook",
		"Book":       "book",
	}
	for in, want := range cases {
		if got := mediaKindFromJF(in); got != want {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
}
