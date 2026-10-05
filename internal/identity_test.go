package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	users map[string]string
	calls atomic.Int32
}

func (f *fakeResolver) ResolveToken(_ context.Context, token string) (*Identity, error) {
	f.calls.Add(1)
	if id, ok := f.users[token]; ok {
		return &Identity{ID: id}, nil
	}
	return nil, nil
}

type fromMuxcoreEnv struct {
	m      *Module
	h      http.Handler
	mu     sync.Mutex
	pushed []string // user ids seen by the Jellyfin fake
}

func newFromMuxcoreEnv(t *testing.T) *fromMuxcoreEnv {
	t.Helper()
	e := &fromMuxcoreEnv{}
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.pushed = append(e.pushed, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		if r.URL.Path == "/Users" {
			_, _ = w.Write([]byte(`[{"Id":"jf-alice","Name":"alice"},{"Id":"jf-bob","Name":"bob"}]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(jf.Close)
	e.m = NewModule(Config{
		ID: "jellyfin-test", GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0",
		BaseURL: jf.URL, APIKey: "k", DataDir: t.TempDir(), WebhookSecret: "s3cret",
		ConflictMode: conflictJellyfin, UserdataPushToJF: true,
		IdentityResolver: &fakeResolver{users: map[string]string{"tok-alice": "alice"}},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /userdata/from-muxcore", e.m.handleUserdataFromMuxcore)
	e.h = mux
	return e
}

func (e *fromMuxcoreEnv) do(hdr map[string]string, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/userdata/from-muxcore"+query, strings.NewReader(`{}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestFromMuxcoreSpoofedHeaderWithoutTokenRejected(t *testing.T) {
	t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "")
	t.Setenv(envTrustCallerHeader, "")
	e := newFromMuxcoreEnv(t)
	if rec := e.do(map[string]string{"X-User-ID": "alice"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	// the webhook secret alone must not make a header identity trusted
	rec := e.do(map[string]string{"X-User-ID": "alice", headerWebhookSecret: "s3cret"}, "?user_id=alice")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	if len(e.pushed) != 0 {
		t.Fatalf("jellyfin was contacted: %v", e.pushed)
	}
}

func TestFromMuxcoreInvalidTokenRejected(t *testing.T) {
	e := newFromMuxcoreEnv(t)
	rec := e.do(map[string]string{"Authorization": "Bearer nope", "X-User-ID": "alice"}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestFromMuxcoreValidToken(t *testing.T) {
	e := newFromMuxcoreEnv(t)
	for name, tc := range map[string]struct {
		hdr   map[string]string
		query string
	}{
		"token only":      {map[string]string{"Authorization": "Bearer tok-alice"}, ""},
		"matching header": {map[string]string{"Authorization": "Bearer tok-alice", "X-User-ID": "alice"}, "?user_id=alice"},
	} {
		rec := e.do(tc.hdr, tc.query)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d (%s), want 200", name, rec.Code, rec.Body.String())
		}
	}
}

func TestFromMuxcoreMismatchedUserForbidden(t *testing.T) {
	e := newFromMuxcoreEnv(t)
	for name, tc := range map[string]struct {
		hdr   map[string]string
		query string
	}{
		"header": {map[string]string{"Authorization": "Bearer tok-alice", "X-User-ID": "bob"}, ""},
		"query":  {map[string]string{"Authorization": "Bearer tok-alice"}, "?user_id=bob"},
	} {
		if rec := e.do(tc.hdr, tc.query); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d, want 403", name, rec.Code)
		}
	}
	if len(e.pushed) != 0 {
		t.Fatalf("jellyfin was contacted: %v", e.pushed)
	}
}

func TestFromMuxcoreLegacyPathGatedByBothEnvVars(t *testing.T) {
	hdr := map[string]string{"X-User-ID": "alice", headerWebhookSecret: "s3cret"}
	for name, env := range map[string][2]string{
		"neither":     {"", ""},
		"insecure":    {"true", ""},
		"trust only":  {"", "1"},
		"trust wrong": {"true", "true"},
	} {
		t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", env[0])
		t.Setenv(envTrustCallerHeader, env[1])
		e := newFromMuxcoreEnv(t)
		if rec := e.do(hdr, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", name, rec.Code)
		}
	}
	t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "true")
	t.Setenv(envTrustCallerHeader, "1")
	e := newFromMuxcoreEnv(t)
	if rec := e.do(hdr, ""); rec.Code != http.StatusOK {
		t.Fatalf("both set: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	// legacy path still needs the webhook secret
	if rec := e.do(map[string]string{"X-User-ID": "alice"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy without secret: got %d, want 401", rec.Code)
	}
}

func TestCachingResolverTTL(t *testing.T) {
	f := &fakeResolver{users: map[string]string{"t": "u"}}
	c := newCachingResolver(f)
	now := time.Now()
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if id, err := c.ResolveToken(context.Background(), "t"); err != nil || id == nil || id.ID != "u" {
			t.Fatalf("resolve: %v %v", id, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", f.calls.Load())
	}
	now = now.Add(31 * time.Second)
	_, _ = c.ResolveToken(context.Background(), "t")
	if f.calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2 after TTL", f.calls.Load())
	}
	// unknown tokens are not cached
	_, _ = c.ResolveToken(context.Background(), "bad")
	_, _ = c.ResolveToken(context.Background(), "bad")
	if f.calls.Load() != 4 {
		t.Fatalf("calls=%d, want 4", f.calls.Load())
	}
}
