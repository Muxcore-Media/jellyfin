package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	discoveryv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/discovery/v1"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/core/sdk/go/module/meshtls"
)

// ADR-0035 (user erasure ledger), jellyfin bridge disposition (§3):
//
//   - userdata_user_map entries whose value is the erased MuxCore user id are
//     deleted;
//   - nothing else. The bridge calls no Jellyfin API for erasure, so Jellyfin
//     server accounts and their watch history are untouched, and map entries
//     keyed by (or valued with) anything other than the erased id — Jellyfin
//     user ids, Jellyfin user names, other users' ids — are retained: they are
//     a different id space and nothing links them to the erased user.
//
// The map stores no tenant, so Tombstone.TenantID is recorded with the applied
// erasure but there is nothing to check it against. User ids are globally
// unique (ADR-0035 §3), so deletion by id is exact.

// CountUserMapEntries is the erasure count key reported to the identity
// provider (matches [a-z0-9_.-]{1,64}).
const CountUserMapEntries = "userdata_user_map"

// erasureRecord is one erasure_applied row. It is stored in settings.json under
// "erasure_applied", keyed by erasure id, in the same file write that removes
// the map entries, so the deletion and its record land together or not at all.
type erasureRecord struct {
	UserID    string           `json:"user_id"`
	TenantID  string           `json:"tenant_id,omitempty"`
	AppliedAt string           `json:"applied_at"`
	Counts    map[string]int64 `json:"counts,omitempty"`
}

// erasureOwner is the bridge's ADR-0035 personal-data owner. Its only input is
// the Reconciler, which reads the verified identity provider's ledger.
type erasureOwner struct {
	m *Module
}

var (
	_ erasure.Owner    = (*erasureOwner)(nil)
	_ erasure.Verifier = (*erasureOwner)(nil)
)

// ModuleID implements erasure.Owner.
func (o *erasureOwner) ModuleID() string { return o.m.id }

// Applied implements erasure.Owner.
func (o *erasureOwner) Applied(ctx context.Context, erasureID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	o.m.mu.RLock()
	defer o.m.mu.RUnlock()
	_, ok := o.m.erased[erasureID]
	return ok, nil
}

// Apply implements erasure.Owner.
func (o *erasureOwner) Apply(ctx context.Context, t erasure.Tombstone) (erasure.Counts, error) {
	counts, err := o.m.eraseUser(ctx, t.ErasureID, t.UserID, t.TenantID)
	if err != nil {
		return nil, err
	}
	return erasure.Counts(counts), nil
}

// Verify implements erasure.Verifier: map entries still valued with the user
// id, in memory and in the persisted file.
func (o *erasureOwner) Verify(ctx context.Context, t erasure.Tombstone) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return o.m.countUserMapEntries(t.UserID)
}

// mapsToUser reports whether a userdata_user_map value is exactly userID.
// Keys (Jellyfin user ids or names) are never matched.
func mapsToUser(value, userID string) bool {
	return userID != "" && strings.TrimSpace(value) == userID
}

// eraseUser applies one tombstone: it removes the user's map entries and
// records erasureID in erasure_applied with ONE settings.json replace. The
// in-memory state changes only after that write succeeded, so on any error
// nothing has changed and nothing is recorded. Applying an erasure id that is
// already recorded is a no-op returning the recorded counts.
func (m *Module) eraseUser(ctx context.Context, erasureID, userID, tenantID string) (map[string]int64, error) {
	if erasureID == "" || userID == "" {
		return nil, errors.New("erasure id and user id are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	if prior, ok := m.erased[erasureID]; ok {
		return maps.Clone(prior.Counts), nil
	}

	kept := make(map[string]string, len(m.userdataUserMap))
	removed := int64(0)
	for k, v := range m.userdataUserMap {
		if mapsToUser(v, userID) {
			removed++
			continue
		}
		kept[k] = v
	}
	counts := map[string]int64{CountUserMapEntries: removed}

	erased := make(map[string]erasureRecord, len(m.erased)+1)
	maps.Copy(erased, m.erased)
	erased[erasureID] = erasureRecord{
		UserID:    userID,
		TenantID:  tenantID,
		AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Counts:    counts,
	}

	s := m.durableSnapshotLocked()
	s.UserdataUserMap = kept
	s.ErasureApplied = erased
	if err := m.writeDurable(s); err != nil {
		return nil, fmt.Errorf("persist erasure: %w", err)
	}
	m.userdataUserMap = kept
	m.erased = erased
	return maps.Clone(counts), nil
}

// countUserMapEntries counts the distinct map keys still valued with userID,
// in memory or in the persisted settings.json (read independently of the
// in-memory state). It is the post-condition of eraseUser and must be 0
// afterwards.
func (m *Module) countUserMapEntries(userID string) (int, error) {
	keys := map[string]struct{}{}
	m.mu.RLock()
	for k, v := range m.userdataUserMap {
		if mapsToUser(v, userID) {
			keys[k] = struct{}{}
		}
	}
	m.mu.RUnlock()

	data, err := os.ReadFile(m.settingsPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return len(keys), nil
	case err != nil:
		return 0, fmt.Errorf("read %s: %w", m.settingsPath(), err)
	}
	var s durableSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, fmt.Errorf("parse %s: %w", m.settingsPath(), err)
	}
	for k, v := range s.UserdataUserMap {
		if mapsToUser(v, userID) {
			keys[k] = struct{}{}
		}
	}
	return len(keys), nil
}

// userErased reports whether userID has an applied erasure. It is read from
// the persisted record on every call, so it survives restarts.
func (m *Module) userErased(userID string) bool {
	if userID == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.erased {
		if rec.UserID == userID {
			return true
		}
	}
	return false
}

// withoutErasedLocked returns in without entries valued with an erased user
// id, so an applied erasure cannot be undone by re-seeding the map from
// USERDATA_USER_MAP, Config or the settings API. in is never mutated. Caller
// holds mu.
func (m *Module) withoutErasedLocked(in map[string]string) map[string]string {
	if len(m.erased) == 0 || len(in) == 0 {
		return in
	}
	ids := make(map[string]struct{}, len(m.erased))
	for _, rec := range m.erased {
		ids[rec.UserID] = struct{}{}
	}
	out := make(map[string]string, len(in))
	dropped := 0
	for k, v := range in {
		if _, gone := ids[strings.TrimSpace(v)]; gone {
			dropped++
			continue
		}
		out[k] = v
	}
	if dropped > 0 {
		slog.Warn("jellyfin: ignored userdata_user_map entries for erased users", "entries", dropped)
	}
	return out
}

// ErasureSweepInterval is the environment variable read for the sweep period.
const ErasureSweepInterval = erasure.EnvSweepInterval

// erasureRequired reports whether the profile makes the reconciler mandatory:
// household (and its alias staging) must not run without it.
func erasureRequired() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MUXCORE_PROFILE"))) {
	case "household", "staging":
		return true
	}
	return false
}

// setupErasure builds the reconciler. It runs from Init, which modulesdk.Run
// calls after mesh enrollment, so MUXCORE_TLS_CERT/KEY/CA point at the
// module's own identity. With a core connection it discovers the exclusive
// identity provider through core; without one the reconciler is not started
// (dev/tests), which household refuses.
func (m *Module) setupErasure() error {
	dialer := m.erasureDialer
	if dialer == nil {
		if strings.TrimSpace(os.Getenv("MUXCORE_GRPC_ADDR")) == "" {
			if erasureRequired() {
				return errors.New("erasure reconciler: household profile requires a core connection (MUXCORE_GRPC_ADDR)")
			}
			slog.Warn("jellyfin erasure reconciler disabled: no core connection (MUXCORE_GRPC_ADDR unset); user erasures from the identity ledger are not applied")
			return nil
		}
		conn, err := modulesdk.Connect(modulesdk.ConnectConfig{Insecure: meshtls.Insecure()})
		if err != nil {
			return fmt.Errorf("erasure reconciler: connect to core: %w", err)
		}
		m.coreConn = conn
		dialer = &erasure.ProviderDialer{Discovery: discoveryClient{conn: conn}}
	}
	cfg := erasure.Config{
		Owner:    &erasureOwner{m: m},
		Dialer:   dialer,
		Logger:   slog.Default(),
		Interval: m.erasureInterval,
	}
	if m.erasureTune != nil {
		m.erasureTune(&cfg)
	}
	rec, err := erasure.New(cfg)
	if err != nil {
		if m.coreConn != nil {
			_ = m.coreConn.Close()
			m.coreConn = nil
		}
		return err
	}
	m.reconciler = rec
	return nil
}

// discoveryClient adapts a core connection to erasure.CapabilityFinder.
type discoveryClient struct{ conn *grpc.ClientConn }

func (d discoveryClient) FindByCapability(ctx context.Context, in *discoveryv1.FindByCapabilityRequest, opts ...grpc.CallOption) (*discoveryv1.FindByCapabilityResponse, error) {
	return discoveryv1.NewDiscoveryServiceClient(d.conn).FindByCapability(ctx, in, opts...)
}

// startErasure runs the reconciler until stopErasure.
func (m *Module) startErasure() {
	if m.reconciler == nil || m.erasureCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.erasureCancel, m.erasureDone = cancel, done
	rec := m.reconciler
	go func() {
		defer close(done)
		if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("jellyfin erasure reconciler stopped", "error", err)
		}
	}()
}

// stopErasure cancels the reconciler and waits for it, so no sweep touches
// settings.json after Stop returns.
func (m *Module) stopErasure(ctx context.Context) {
	if m.erasureCancel != nil {
		m.erasureCancel()
		select {
		case <-m.erasureDone:
		case <-ctx.Done():
			slog.Warn("jellyfin erasure reconciler did not stop before the shutdown deadline")
		}
		m.erasureCancel, m.erasureDone = nil, nil
	}
	m.reconciler = nil
	if m.coreConn != nil {
		_ = m.coreConn.Close()
		m.coreConn = nil
	}
}

// Reconciler exposes the erasure reconciler (nil when disabled) for tests and
// operator triggers.
func (m *Module) Reconciler() *erasure.Reconciler { return m.reconciler }
