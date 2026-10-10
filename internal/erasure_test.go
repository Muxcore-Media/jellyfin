package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	discoveryv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/discovery/v1"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure/erasuretest"
	"github.com/Muxcore-Media/core/sdk/go/module/meshtls"
)

const testOwnerID = "jellyfin"

func clearMeshEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_DEV_TLS_SKIP", "MUXCORE_GRPC_INSECURE",
		meshtls.EnvTLSCert, meshtls.EnvTLSKey, meshtls.EnvTLSCA, meshtls.EnvTLSServerName,
		"MUXCORE_PROFILE", "MUXCORE_MESH_DIAL_LOCAL", "MUXCORE_GRPC_ADDR", erasure.EnvSweepInterval,
		"USERDATA_USER_MAP", "JELLYFIN_BASE_URL", "JELLYFIN_URL", "JELLYFIN_API_KEY"} {
		t.Setenv(k, "")
	}
}

func devEnv(t *testing.T) {
	t.Helper()
	clearMeshEnv(t)
	t.Setenv("MUXCORE_PROFILE", "dev")
	t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "true")
}

func fastTune(c *erasure.Config) {
	c.AckBackoff = time.Millisecond
	c.RetryBackoff = 50 * time.Millisecond
	c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	c.CallTimeout = 5 * time.Second
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeCore serves a DiscoveryService answering the identity capability.
type fakeCore struct {
	discoveryv1.UnimplementedDiscoveryServiceServer
	disc *erasuretest.Discovery
}

func (f fakeCore) FindByCapability(ctx context.Context, in *discoveryv1.FindByCapabilityRequest) (*discoveryv1.FindByCapabilityResponse, error) {
	return f.disc.FindByCapability(ctx, in)
}

func serveFakeCore(t *testing.T, disc *erasuretest.Discovery) string {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	discoveryv1.RegisterDiscoveryServiceServer(gs, fakeCore{disc: disc})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// seedMap is the household's mapping: alice and bob each have a Jellyfin id
// and a name entry; "jf-guest" is a Jellyfin-only account whose value is not a
// MuxCore user id.
func seedMap() map[string]string {
	return map[string]string{
		"jf-alice-id": "mux-alice",
		"Alice":       "mux-alice",
		"jf-bob-id":   "mux-bob",
		"Bob":         "mux-bob",
		"jf-guest-id": "guest",
	}
}

func erasureModule(t *testing.T, dir string, cfg Config) *Module {
	t.Helper()
	cfg.DataDir = dir
	cfg.GRPCAddr, cfg.HTTPAddr = "127.0.0.1:0", "127.0.0.1:0"
	return NewModule(cfg)
}

// seededModule initialises a module whose settings.json already holds seedMap.
func seededModule(t *testing.T, dir string, extra Config) *Module {
	t.Helper()
	extra.UserdataUserMap = seedMap()
	m := erasureModule(t, dir, extra)
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.persistDurable(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

func readSettings(t *testing.T, dir string) durableSettings {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s durableSettings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireMap(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("map = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("map = %v, want %v", got, want)
		}
	}
}

func without(in map[string]string, keys ...string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func tombstone(erasureID, userID, tenantID string) erasure.Tombstone {
	return erasure.Tombstone{ErasureID: erasureID, UserID: userID, TenantID: tenantID, DeletedAt: time.Now()}
}

// TestErasureLifecycleThroughCoreDiscovery runs the real wiring: the module
// connects to a (fake) core, discovers the identity provider through it,
// sweeps at startup, survives a restart and stops its reconciler on Stop.
func TestErasureLifecycleThroughCoreDiscovery(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	seeder := seededModule(t, dir, Config{})
	_ = seeder.Stop(context.Background())

	provider := erasuretest.NewProvider()
	provider.Allowed = map[string]bool{testOwnerID: true}
	providerAddr := erasuretest.ServePlain(t, provider)
	coreAddr := serveFakeCore(t, erasuretest.NewDiscovery(erasuretest.Module("auth-local", providerAddr)))
	t.Setenv("MUXCORE_GRPC_ADDR", coreAddr)
	t.Setenv(erasure.EnvSweepInterval, "1m")
	eid := provider.AddErasure("mux-alice", "home")

	ctx := context.Background()
	m := erasureModule(t, dir, Config{ErasureTune: fastTune})
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Reconciler() == nil {
		t.Fatal("reconciler not configured despite a core connection")
	}
	// Start also dials the event bus, which this fake core does not serve;
	// start only the reconciler, exactly as Start does last.
	m.startErasure()
	done := m.erasureDone
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = m.Stop(ctx)
		}
	})

	eventually(t, "startup sweep acknowledgement", func() bool {
		a, ok := provider.Latest(eid, testOwnerID)
		return ok && a.Outcome == authv1.ErasureOutcome_ERASURE_OUTCOME_OK
	})
	ack, _ := provider.Latest(eid, testOwnerID)
	if ack.Counts[CountUserMapEntries] != 2 {
		t.Errorf("ack counts = %v", ack.Counts)
	}
	s := readSettings(t, dir)
	requireMap(t, s.UserdataUserMap, without(seedMap(), "jf-alice-id", "Alice"))
	rec, ok := s.ErasureApplied[eid]
	if !ok || rec.UserID != "mux-alice" || rec.TenantID != "home" || rec.Counts[CountUserMapEntries] != 2 {
		t.Errorf("erasure_applied = %+v", s.ErasureApplied)
	}

	// Second sweep: nothing is applied or deleted again.
	res, err := m.Reconciler().SweepOnce(ctx)
	if err != nil || res.Applied != 0 || res.Skipped != 1 {
		t.Fatalf("second sweep: %+v %v", res, err)
	}

	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	select {
	case <-done:
	default:
		t.Fatal("reconciler goroutine still running after Stop")
	}
	if m.Reconciler() != nil || m.coreConn != nil {
		t.Fatal("reconciler or core connection retained after Stop")
	}

	// Restart with no provider and with the env map re-seeding the erased
	// user: the persisted record, not memory, keeps the entries out.
	t.Setenv("MUXCORE_GRPC_ADDR", "")
	t.Setenv("USERDATA_USER_MAP", "jf-alice-id:mux-alice,jf-bob-id:mux-bob")
	m2 := erasureModule(t, dir, Config{})
	if err := m2.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Stop(ctx) })
	if m2.Reconciler() != nil {
		t.Fatal("reconciler started without a core connection")
	}
	requireMap(t, m2.userdataUserMap, map[string]string{"jf-bob-id": "mux-bob"})
	if !m2.userErased("mux-alice") || m2.userErased("mux-bob") {
		t.Fatal("erased-user record not reloaded")
	}
}

func TestEraseUserDeletesOnlyMatchingValue(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	m := seededModule(t, dir, Config{})
	// Entries that must survive a "mux-alice" erasure even though they look
	// related: a key equal to the erased id (keys are Jellyfin ids/names, a
	// different id space), a Jellyfin-only id with no MuxCore link, and a
	// near-miss value.
	if err := m.updateSetting("userdata_user_map", formatUserMap(map[string]string{
		"jf-alice-id": "mux-alice",
		"Alice":       "mux-alice",
		"jf-bob-id":   "mux-bob",
		"Bob":         "mux-bob",
		"jf-guest-id": "guest",
		"mux-alice":   "mux-bob",
		"jf-only-id":  "jf-only-id",
		"jf-near":     "mux-alice2",
	})); err != nil {
		t.Fatal(err)
	}

	owner := &erasureOwner{m: m}
	counts, err := owner.Apply(context.Background(), tombstone("er-1", "mux-alice", ""))
	if err != nil {
		t.Fatal(err)
	}
	if counts[CountUserMapEntries] != 2 {
		t.Fatalf("counts = %v", counts)
	}
	want := map[string]string{
		"jf-bob-id":   "mux-bob",
		"Bob":         "mux-bob",
		"jf-guest-id": "guest",
		"mux-alice":   "mux-bob",
		"jf-only-id":  "jf-only-id",
		"jf-near":     "mux-alice2",
	}
	requireMap(t, m.userdataUserMap, want)
	requireMap(t, readSettings(t, dir).UserdataUserMap, want)

	n, err := owner.Verify(context.Background(), tombstone("er-1", "mux-alice", ""))
	if err != nil || n != 0 {
		t.Fatalf("verify = %d, %v", n, err)
	}
}

func TestEraseUserReapplyIsNoOp(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	m := seededModule(t, dir, Config{})
	owner := &erasureOwner{m: m}
	ctx := context.Background()

	first, err := owner.Apply(ctx, tombstone("er-1", "mux-alice", "home"))
	if err != nil || first[CountUserMapEntries] != 2 {
		t.Fatalf("first apply: %v %v", first, err)
	}
	if ok, err := owner.Applied(ctx, "er-1"); err != nil || !ok {
		t.Fatalf("applied = %v, %v", ok, err)
	}
	if ok, _ := owner.Applied(ctx, "er-other"); ok {
		t.Fatal("unrelated erasure id reported applied")
	}
	before := readSettings(t, dir)

	// New entries for another user appear between the two applications. Even
	// a re-delivery that names a different user id under the same erasure id
	// must not delete anything: the erasure id is the record.
	if err := m.updateSetting("userdata_user_map", "jf-carol-id:mux-carol,jf-bob-id:mux-bob"); err != nil {
		t.Fatal(err)
	}
	second, err := owner.Apply(ctx, tombstone("er-1", "mux-carol", "home"))
	if err != nil {
		t.Fatal(err)
	}
	if second[CountUserMapEntries] != 2 {
		t.Fatalf("re-apply must return the recorded counts, got %v", second)
	}
	requireMap(t, m.userdataUserMap, map[string]string{"jf-carol-id": "mux-carol", "jf-bob-id": "mux-bob"})
	after := readSettings(t, dir)
	if len(after.ErasureApplied) != 1 || after.ErasureApplied["er-1"].UserID != before.ErasureApplied["er-1"].UserID ||
		after.ErasureApplied["er-1"].AppliedAt != before.ErasureApplied["er-1"].AppliedAt {
		t.Fatalf("record changed on re-apply: %+v -> %+v", before.ErasureApplied, after.ErasureApplied)
	}
}

func TestEraseUserWithNoEntriesStillRecords(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	m := seededModule(t, dir, Config{})
	owner := &erasureOwner{m: m}
	counts, err := owner.Apply(context.Background(), tombstone("er-1", "mux-nobody", ""))
	if err != nil || counts[CountUserMapEntries] != 0 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	requireMap(t, m.userdataUserMap, seedMap())
	if _, ok := readSettings(t, dir).ErasureApplied["er-1"]; !ok {
		t.Fatal("erasure with zero matching entries was not recorded")
	}
}

func TestEraseUserRejectsEmptyIDs(t *testing.T) {
	devEnv(t)
	m := seededModule(t, t.TempDir(), Config{})
	for _, tc := range []struct{ eid, uid string }{{"", "mux-alice"}, {"er-1", ""}} {
		if _, err := m.eraseUser(context.Background(), tc.eid, tc.uid, ""); err == nil {
			t.Errorf("eraseUser(%q,%q) accepted", tc.eid, tc.uid)
		}
	}
	requireMap(t, m.userdataUserMap, seedMap())
}

// TestPersistFailureDoesNotRecordApplied makes the settings.json replace fail
// (the temp path is a directory). Nothing may change in memory or on disk and
// no erasure_applied record may exist; the next sweep completes.
func TestPersistFailureDoesNotRecordApplied(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	provider := erasuretest.NewProvider()
	providerAddr := erasuretest.ServePlain(t, provider)
	dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery(erasuretest.Module("auth-local", providerAddr))}
	eid := provider.AddErasure("mux-alice", "")

	m := seededModule(t, dir, Config{ErasureDialer: dialer, ErasureTune: fastTune})
	ctx := context.Background()
	diskBefore, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	block := filepath.Join(dir, "settings.json.tmp")
	if err := os.Mkdir(block, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := m.Reconciler().SweepOnce(ctx)
	if err == nil || res.Failed != 1 || res.Applied != 0 {
		t.Fatalf("sweep with persist failure: %+v %v", res, err)
	}
	a, ok := provider.Latest(eid, testOwnerID)
	if !ok || a.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_FAILED || a.Detail != erasure.DetailApplyFailed {
		t.Fatalf("ack after failure: %+v %v", a, ok)
	}
	requireMap(t, m.userdataUserMap, seedMap())
	if applied, _ := (&erasureOwner{m: m}).Applied(ctx, eid); applied {
		t.Fatal("erasure recorded as applied after a failed persist")
	}
	diskAfter, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(diskAfter) != string(diskBefore) {
		t.Fatalf("settings.json changed by a failed apply:\n%s", diskAfter)
	}

	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	res, err = m.Reconciler().SweepOnce(ctx)
	if err != nil || res.Applied != 1 || res.Acked != 1 {
		t.Fatalf("next sweep: %+v %v", res, err)
	}
	if a, _ := provider.Latest(eid, testOwnerID); a.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_OK {
		t.Fatalf("ack after retry: %+v", a)
	}
	requireMap(t, readSettings(t, dir).UserdataUserMap, without(seedMap(), "jf-alice-id", "Alice"))
}

func TestVerifyCountsRemainingEntriesInMemoryAndOnDisk(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	m := seededModule(t, dir, Config{})
	owner := &erasureOwner{m: m}
	ctx := context.Background()

	n, err := owner.Verify(ctx, tombstone("er-1", "mux-alice", ""))
	if err != nil || n != 2 {
		t.Fatalf("before erasure: %d, %v", n, err)
	}
	// An entry present only in the persisted file still counts.
	m.mu.Lock()
	m.userdataUserMap = map[string]string{}
	m.mu.Unlock()
	if n, err := owner.Verify(ctx, tombstone("er-1", "mux-alice", "")); err != nil || n != 2 {
		t.Fatalf("on-disk entries: %d, %v", n, err)
	}
	if n, err := owner.Verify(ctx, tombstone("er-1", "mux-bob", "")); err != nil || n != 2 {
		t.Fatalf("other user: %d, %v", n, err)
	}
	if n, err := owner.Verify(ctx, tombstone("er-1", "nobody", "")); err != nil || n != 0 {
		t.Fatalf("absent user: %d, %v", n, err)
	}
}

func TestSetupErasureConfiguration(t *testing.T) {
	ctx := context.Background()
	t.Run("household requires a core connection", func(t *testing.T) {
		clearMeshEnv(t)
		t.Setenv("MUXCORE_PROFILE", "household")
		m := erasureModule(t, t.TempDir(), Config{})
		if err := m.Init(ctx); err == nil {
			t.Fatal("household without a core connection started")
		}
		if m.lis != nil || m.httpLis != nil {
			t.Fatal("listeners left open after a failed Init")
		}
		if m.Reconciler() != nil {
			t.Fatal("reconciler present after a failed Init")
		}
	})
	t.Run("staging requires a core connection", func(t *testing.T) {
		clearMeshEnv(t)
		t.Setenv("MUXCORE_PROFILE", "staging")
		m := erasureModule(t, t.TempDir(), Config{})
		if err := m.setupErasure(); err == nil {
			t.Fatal("staging without a core connection started")
		}
	})
	t.Run("dev without core disables with a warning", func(t *testing.T) {
		devEnv(t)
		m := erasureModule(t, t.TempDir(), Config{})
		if err := m.setupErasure(); err != nil || m.Reconciler() != nil {
			t.Fatalf("dev: %v %v", err, m.Reconciler())
		}
	})
	t.Run("invalid sweep interval is a startup error", func(t *testing.T) {
		devEnv(t)
		t.Setenv(erasure.EnvSweepInterval, "soon")
		dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery()}
		m := erasureModule(t, t.TempDir(), Config{ErasureDialer: dialer})
		if err := m.setupErasure(); err == nil {
			t.Fatal("invalid ERASURE_SWEEP_INTERVAL accepted")
		}
	})
	t.Run("interval from environment", func(t *testing.T) {
		devEnv(t)
		t.Setenv(erasure.EnvSweepInterval, "2m")
		dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery()}
		m := erasureModule(t, t.TempDir(), Config{ErasureDialer: dialer})
		if err := m.setupErasure(); err != nil || m.Reconciler() == nil {
			t.Fatalf("setup: %v", err)
		}
		m.stopErasure(ctx)
	})
}

// TestUpgradeFromSettingsSnapshots opens settings.json files written by
// earlier tags with the current code (ADR-0015): the module still opens, the
// erasure_applied record starts empty, existing map entries and item links
// survive, and an erasure applied on top only removes the erased user.
// Fixtures: v0.3.5 was generated by that tag's persistDurable; v0.2.0
// predates the userdata fields (its struct was base_url…sessions_poll_seconds
// and links).
func TestUpgradeFromSettingsSnapshots(t *testing.T) {
	for _, tc := range []struct {
		tag     string
		wantMap map[string]string
	}{
		{"v0.2.0", map[string]string{}},
		{"v0.3.5", map[string]string{
			"0a1b2c3d4e5f60718293a4b5c6d7e8f9": "mux-user-alice",
			"Bob":                              "mux-user-bob",
			"jf-only-guest":                    "guest",
		}},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			devEnv(t)
			dir := t.TempDir()
			src, err := os.ReadFile(filepath.Join("testdata", "upgrade", "settings-"+tc.tag+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), src, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()

			// Open twice: startup must be idempotent.
			for i := 0; i < 2; i++ {
				m := erasureModule(t, dir, Config{})
				if err := m.Init(ctx); err != nil {
					t.Fatalf("open %d: %v", i, err)
				}
				if len(m.erased) != 0 {
					t.Fatalf("erasure_applied not empty after upgrade: %v", m.erased)
				}
				if m.baseURL != "http://jellyfin:8096" || m.apiKey != "fixture-api-key" || m.conflictMode != conflictMuxcore || m.sessionsPollSec != 30 {
					t.Fatalf("settings lost: %q %q %q %d", m.baseURL, m.apiKey, m.conflictMode, m.sessionsPollSec)
				}
				requireMap(t, m.userdataUserMap, tc.wantMap)
				if l := m.links["mux-movie-1"]; l == nil || l.JellyfinID != "jf-item-1" || l.Title != "Heat" {
					t.Fatalf("links lost: %+v", m.links)
				}
				if err := m.persistDurable(); err != nil {
					t.Fatal(err)
				}
				if s := readSettings(t, dir); len(s.ErasureApplied) != 0 {
					t.Fatalf("empty erasure table not empty on disk: %v", s.ErasureApplied)
				}
				_ = m.Stop(ctx)
			}

			if tc.tag != "v0.3.5" {
				return
			}
			m := erasureModule(t, dir, Config{})
			if err := m.Init(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Stop(ctx) })
			counts, err := (&erasureOwner{m: m}).Apply(ctx, tombstone("er-1", "mux-user-alice", ""))
			if err != nil || counts[CountUserMapEntries] != 1 {
				t.Fatalf("apply on upgraded settings: %v %v", counts, err)
			}
			s := readSettings(t, dir)
			requireMap(t, s.UserdataUserMap, map[string]string{"Bob": "mux-user-bob", "jf-only-guest": "guest"})
			if s.BaseURL != "http://jellyfin:8096" || len(s.Links) != 1 {
				t.Fatalf("unrelated settings changed: %+v", s)
			}
		})
	}
}

// TestSettingsUpdateCannotReseedErasedUser: the settings API (and env) must
// not put an erased user id back into the map.
func TestSettingsUpdateCannotReseedErasedUser(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	m := seededModule(t, dir, Config{})
	if _, err := (&erasureOwner{m: m}).Apply(context.Background(), tombstone("er-1", "mux-alice", "")); err != nil {
		t.Fatal(err)
	}
	if err := m.updateSetting("userdata_user_map", "jf-alice-id:mux-alice,jf-bob-id:mux-bob"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"jf-bob-id": "mux-bob"}
	requireMap(t, m.userdataUserMap, want)
	requireMap(t, readSettings(t, dir).UserdataUserMap, want)
}

// TestErasedUserWritesRefused: bridge writes carry no user bearer, so an id
// the ledger erased is refused, others still go through.
func TestErasedUserWritesRefused(t *testing.T) {
	devEnv(t)
	m := seededModule(t, t.TempDir(), Config{})
	var mu sync.Mutex
	var published int
	testPublishHook = func(context.Context, string, string, []byte) error {
		mu.Lock()
		defer mu.Unlock()
		published++
		return nil
	}
	t.Cleanup(func() { testPublishHook = nil })

	ctx := context.Background()
	if _, err := (&erasureOwner{m: m}).Apply(ctx, tombstone("er-1", "mux-alice", "")); err != nil {
		t.Fatal(err)
	}
	if err := m.writeMuxUserdata(ctx, "mux-alice", muxUserdataBlob{}); err == nil {
		t.Fatal("write for an erased user accepted")
	}
	if err := m.writeMuxUserdata(ctx, "mux-bob", muxUserdataBlob{}); err != nil {
		t.Fatalf("write for another user: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if published != 1 {
		t.Fatalf("published %d events, want 1 (bob only)", published)
	}
}

// TestEraseUserConcurrentWithSettingsUpdates is for -race: an erasure and
// settings writes interleave, and the record is never rolled back by a stale
// snapshot.
func TestEraseUserConcurrentWithSettingsUpdates(t *testing.T) {
	devEnv(t)
	dir := t.TempDir()
	m := seededModule(t, dir, Config{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if err := m.updateSetting("conflict_mode", "muxcore"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := m.eraseUser(context.Background(), "er-1", "mux-alice", ""); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	wg.Wait()
	s := readSettings(t, dir)
	if _, ok := s.ErasureApplied["er-1"]; !ok {
		t.Fatal("erasure record lost to a concurrent settings write")
	}
	requireMap(t, s.UserdataUserMap, without(seedMap(), "jf-alice-id", "Alice"))
}
