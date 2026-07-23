package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/Muxcore-Media/core/pkg/contracts"
	eventsv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/events/v1"
	"github.com/Muxcore-Media/core/sdk/go/client"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	jellyfinv1 "github.com/Muxcore-Media/jellyfin/proto/jellyfinv1"
)

const (
	moduleVersion = "0.2.0"

	conflictJellyfin = "jellyfin"
	conflictMuxcore  = "muxcore"
	conflictManual   = "manual"

	headerWebhookSecret = "X-Jellyfin-Webhook-Secret"
)

type Module struct {
	jellyfinv1.UnimplementedJellyfinBridgeServer

	mu              sync.RWMutex
	baseURL         string
	apiKey          string
	webhookSecret   string
	conflictMode    string
	sessionsPollSec int
	id              string
	grpcAddr        string
	httpAddr        string
	dataDir         string
	grpcSrv         *grpc.Server
	lis             net.Listener
	httpSrv         *http.Server
	httpLis         net.Listener
	httpCli         *http.Client
	mc              *client.Client
	links           map[string]*ItemLink // key: muxcore_id or jf:jellyfin_id
	stopCh          chan struct{}
	sessionSeen     map[string]string // sessionKey -> last state
}

type Config struct {
	ID              string
	GRPCAddr        string
	HTTPAddr        string
	BaseURL         string
	APIKey          string
	WebhookSecret   string
	DataDir         string
	ConflictMode    string
	SessionsPollSec int
}

type durableSettings struct {
	BaseURL         string              `json:"base_url"`
	APIKey          string              `json:"api_key"`
	WebhookSecret   string              `json:"webhook_secret"`
	ConflictMode    string              `json:"conflict_mode"`
	SessionsPollSec int                 `json:"sessions_poll_seconds"`
	Links           map[string]ItemLink `json:"links,omitempty"`
}

// ItemLink is the durable MuxCore ↔ Jellyfin mapping.
type ItemLink struct {
	MuxcoreID   string            `json:"muxcore_id"`
	JellyfinID  string            `json:"jellyfin_id"`
	Path        string            `json:"path,omitempty"`
	ProviderIDs map[string]string `json:"provider_ids,omitempty"`
	MediaKind   string            `json:"media_kind,omitempty"`
	Title       string            `json:"title,omitempty"`
	UpdatedAt   int64             `json:"updated_at_unix"`
}

func NewModule(cfg Config) *Module {
	if cfg.ID == "" {
		cfg.ID = "jellyfin"
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = ":9475"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8475"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/muxcore-jellyfin"
	}
	if cfg.ConflictMode == "" {
		cfg.ConflictMode = conflictJellyfin
	}
	if v := os.Getenv("JELLYFIN_GRPC_ADDR"); v != "" {
		cfg.GRPCAddr = v
	}
	if v := os.Getenv("JELLYFIN_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("JELLYFIN_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv("JELLYFIN_BASE_URL")
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("JELLYFIN_API_KEY")
	}
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = os.Getenv("JELLYFIN_WEBHOOK_SECRET")
	}
	if v := os.Getenv("JELLYFIN_CONFLICT_MODE"); v != "" {
		cfg.ConflictMode = v
	}
	if cfg.SessionsPollSec == 0 {
		if v := os.Getenv("JELLYFIN_SESSIONS_POLL_SECONDS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				cfg.SessionsPollSec = n
			}
		}
	}
	m := &Module{
		id:              cfg.ID,
		grpcAddr:        cfg.GRPCAddr,
		httpAddr:        cfg.HTTPAddr,
		dataDir:         cfg.DataDir,
		baseURL:         strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		apiKey:          strings.TrimSpace(cfg.APIKey),
		webhookSecret:   strings.TrimSpace(cfg.WebhookSecret),
		conflictMode:    normalizeConflict(cfg.ConflictMode),
		sessionsPollSec: cfg.SessionsPollSec,
		httpCli:         &http.Client{Timeout: 20 * time.Second},
		links:           map[string]*ItemLink{},
		stopCh:          make(chan struct{}),
		sessionSeen:     map[string]string{},
	}
	return m
}

func normalizeConflict(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case conflictMuxcore:
		return conflictMuxcore
	case conflictManual:
		return conflictManual
	default:
		return conflictJellyfin
	}
}

func (m *Module) Info() contracts.ModuleInfo {
	return contracts.ModuleInfo{
		ID:             m.id,
		Name:           "Jellyfin Playback Bridge",
		Version:        moduleVersion,
		Roles:          []string{"playback"},
		Description:    "Jellyfin bridge — library refresh/sync, playback session events, external play deep-links",
		Author:         "MuxCore",
		Capabilities:   []string{"playback.jellyfin", "playback", "settings"},
		MinCoreVersion: "0.4.0",
		HTTPAddr:       m.grpcAddr,
	}
}

func (m *Module) Init(ctx context.Context) error {
	if err := os.MkdirAll(m.dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w", m.dataDir, err)
	}
	if err := m.loadDurable(); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	lis, err := net.Listen("tcp", m.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen gRPC %s: %w", m.grpcAddr, err)
	}
	m.lis = lis
	httpLis, err := net.Listen("tcp", m.httpAddr)
	if err != nil {
		lis.Close()
		return fmt.Errorf("listen HTTP %s: %w", m.httpAddr, err)
	}
	m.httpLis = httpLis
	slog.Info("jellyfin bridge initialized", "grpc", m.grpcAddr, "http", m.httpAddr, "data_dir", m.dataDir, "configured", m.configured())
	return nil
}

func (m *Module) Start(ctx context.Context) error {
	m.grpcSrv = grpc.NewServer()
	jellyfinv1.RegisterJellyfinBridgeServer(m.grpcSrv, m)
	modulesdk.RegisterMeshHandler(m.grpcSrv, m.id, modulesdk.SettingsHandler{
		List:   m.settingsDefs,
		Update: m.updateSetting,
	})
	go func() {
		if err := m.grpcSrv.Serve(m.lis); err != nil {
			slog.Error("jellyfin gRPC serve", "error", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", m.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	m.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := m.httpSrv.Serve(m.httpLis); err != nil && err != http.ErrServerClosed {
			slog.Error("jellyfin HTTP serve", "error", err)
		}
	}()

	go m.connectCoreAndSubscribe()
	go m.pollSessionsLoop()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	if m.httpSrv != nil {
		_ = m.httpSrv.Shutdown(ctx)
	}
	if m.grpcSrv != nil {
		m.grpcSrv.GracefulStop()
	}
	m.mu.Lock()
	mc := m.mc
	m.mc = nil
	m.mu.Unlock()
	if mc != nil {
		mc.Close()
	}
	return nil
}

func (m *Module) Health(ctx context.Context) error {
	if !m.configured() {
		return fmt.Errorf("jellyfin not configured")
	}
	return m.probeSystemInfo(ctx)
}

func (m *Module) configured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.baseURL != "" && m.apiKey != ""
}

func (m *Module) settingsPath() string {
	return filepath.Join(m.dataDir, "settings.json")
}

func (m *Module) loadDurable() error {
	path := m.settingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m.persistDurable()
		}
		return err
	}
	var s durableSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.baseURL == "" && s.BaseURL != "" {
		m.baseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	}
	if m.apiKey == "" && s.APIKey != "" {
		m.apiKey = strings.TrimSpace(s.APIKey)
	}
	if m.webhookSecret == "" && s.WebhookSecret != "" {
		m.webhookSecret = strings.TrimSpace(s.WebhookSecret)
	}
	if s.ConflictMode != "" {
		m.conflictMode = normalizeConflict(s.ConflictMode)
	}
	if s.SessionsPollSec > 0 {
		m.sessionsPollSec = s.SessionsPollSec
	}
	m.links = map[string]*ItemLink{}
	for k, link := range s.Links {
		cp := link
		m.links[k] = &cp
	}
	return nil
}

func (m *Module) persistDurable() error {
	m.mu.RLock()
	s := durableSettings{
		BaseURL:         m.baseURL,
		APIKey:          m.apiKey,
		WebhookSecret:   m.webhookSecret,
		ConflictMode:    m.conflictMode,
		SessionsPollSec: m.sessionsPollSec,
		Links:           map[string]ItemLink{},
	}
	for k, link := range m.links {
		if link == nil {
			continue
		}
		s.Links[k] = *link
	}
	m.mu.RUnlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.settingsPath())
}

func (m *Module) settingsDefs() []contracts.SettingDef {
	m.mu.RLock()
	base, key, secret, conflict, poll := m.baseURL, m.apiKey, m.webhookSecret, m.conflictMode, m.sessionsPollSec
	m.mu.RUnlock()
	return []contracts.SettingDef{
		{
			Key: "base_url", Label: "Jellyfin Base URL", Type: contracts.SettingTypeString,
			Value: base, Description: "e.g. http://jellyfin:8096", Required: true, Group: "Connection",
		},
		{
			Key: "api_key", Label: "Jellyfin API Key", Type: contracts.SettingTypeSecret,
			Value: modulesdk.MaskSecret(key), Description: "API key from Jellyfin dashboard", Required: true, Group: "Connection",
		},
		{
			Key: "webhook_secret", Label: "Webhook Shared Secret", Type: contracts.SettingTypeSecret,
			Value: modulesdk.MaskSecret(secret), Description: "Required in X-Jellyfin-Webhook-Secret or Authorization: Bearer when set", Required: false, Group: "Security",
		},
		{
			Key: "conflict_mode", Label: "Library Sync Conflict Mode", Type: contracts.SettingTypeString,
			Value: conflict, Description: "jellyfin | muxcore | manual — which side wins metadata on conflict", Required: false, Group: "Sync",
		},
		{
			Key: "sessions_poll_seconds", Label: "Sessions Poll Interval (seconds)", Type: contracts.SettingTypeInt,
			Value: fmt.Sprintf("%d", poll), Description: "0 disables /Sessions polling; webhooks preferred", Required: false, Group: "Playback",
		},
	}
}

func (m *Module) updateSetting(key, value string) error {
	switch key {
	case "base_url", "JELLYFIN_BASE_URL":
		m.mu.Lock()
		m.baseURL = strings.TrimRight(strings.TrimSpace(value), "/")
		m.mu.Unlock()
	case "api_key", "JELLYFIN_API_KEY":
		if value == "********" {
			return nil
		}
		m.mu.Lock()
		m.apiKey = strings.TrimSpace(value)
		m.mu.Unlock()
	case "webhook_secret", "JELLYFIN_WEBHOOK_SECRET":
		if value == "********" {
			return nil
		}
		m.mu.Lock()
		m.webhookSecret = strings.TrimSpace(value)
		m.mu.Unlock()
	case "conflict_mode", "JELLYFIN_CONFLICT_MODE":
		m.mu.Lock()
		m.conflictMode = normalizeConflict(value)
		m.mu.Unlock()
	case "sessions_poll_seconds", "JELLYFIN_SESSIONS_POLL_SECONDS":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return fmt.Errorf("invalid sessions_poll_seconds")
		}
		m.mu.Lock()
		m.sessionsPollSec = n
		m.mu.Unlock()
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return m.persistDurable()
}

func (m *Module) RefreshLibrary(ctx context.Context, req *jellyfinv1.RefreshLibraryRequest) (*jellyfinv1.RefreshLibraryResponse, error) {
	if err := m.refreshLibrary(ctx, req.GetItemId()); err != nil {
		return nil, err
	}
	return &jellyfinv1.RefreshLibraryResponse{Ok: true}, nil
}

func (m *Module) PlayURL(_ context.Context, req *jellyfinv1.PlayURLRequest) (*jellyfinv1.PlayURLResponse, error) {
	m.mu.RLock()
	base := m.baseURL
	m.mu.RUnlock()
	if base == "" || req.GetItemId() == "" {
		return nil, fmt.Errorf("base_url and item_id required")
	}
	url := fmt.Sprintf("%s/web/index.html#!/details?id=%s", base, req.GetItemId())
	return &jellyfinv1.PlayURLResponse{Url: url}, nil
}

func (m *Module) Status(context.Context, *jellyfinv1.StatusRequest) (*jellyfinv1.StatusResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, link := range m.links {
		if link != nil && link.MuxcoreID != "" {
			n++
		}
	}
	return &jellyfinv1.StatusResponse{
		Configured:          m.baseURL != "" && m.apiKey != "",
		BaseUrl:             m.baseURL,
		ConflictMode:        m.conflictMode,
		ItemLinks:           int32(n),
		SessionsPollEnabled: m.sessionsPollSec > 0,
	}, nil
}

func (m *Module) connectCoreAndSubscribe() {
	addr := os.Getenv("MUXCORE_GRPC_ADDR")
	if addr == "" {
		return
	}
	var opts []client.Option
	if os.Getenv("MUXCORE_INSECURE_DISABLE_TLS") == "true" || os.Getenv("MUXCORE_GRPC_INSECURE") == "true" {
		opts = append(opts, client.WithInsecure())
	}
	backoff := time.Second
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}
		c, err := client.Dial(addr, opts...)
		if err != nil {
			slog.Warn("jellyfin: dial core failed, retrying", "error", err, "backoff", backoff)
			select {
			case <-m.stopCh:
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		m.mu.Lock()
		if m.mc != nil {
			m.mc.Close()
		}
		m.mc = c
		m.mu.Unlock()
		slog.Info("jellyfin: connected to core mesh", "addr", addr)
		m.subscribeImportEvents()
		return
	}
}

func (m *Module) eventClient() *client.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mc
}

// publishEvent publishes to the core event bus. Tests may override via testPublishHook.
var testPublishHook func(ctx context.Context, eventType, source string, payload []byte) error

func (m *Module) publishEvent(ctx context.Context, eventType string, payload []byte) error {
	if testPublishHook != nil {
		return testPublishHook(ctx, eventType, m.id, payload)
	}
	mc := m.eventClient()
	if mc == nil {
		return nil
	}
	return mc.Events.Publish(ctx, eventType, m.id, payload)
}

func (m *Module) subscribeImportEvents() {
	mc := m.eventClient()
	if mc == nil {
		return
	}
	for _, et := range []string{"download.completed", "media.imported", contracts.EventFileImported} {
		ch, cancel, err := mc.Events.Subscribe(context.Background(), et)
		if err != nil {
			slog.Debug("jellyfin: subscribe failed", "type", et, "error", err)
			continue
		}
		go func(events <-chan *eventsv1.Event, eventType string, cancel context.CancelFunc) {
			defer cancel()
			for evt := range events {
				if err := m.refreshLibrary(context.Background(), ""); err != nil {
					slog.Debug("jellyfin: refresh on event failed", "type", eventType, "error", err)
				} else {
					slog.Info("jellyfin: library refreshed", "trigger", eventType)
				}
				m.handleImportEvent(eventType, evt)
			}
		}(ch, et, cancel)
	}
}

func (m *Module) handleImportEvent(eventType string, evt *eventsv1.Event) {
	if evt == nil || len(evt.Payload) == 0 {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return
	}
	muxID, _ := payload["id"].(string)
	if muxID == "" {
		muxID, _ = payload["item_id"].(string)
	}
	path, _ := payload["path"].(string)
	if path == "" {
		path, _ = payload["file_path"].(string)
	}
	if muxID == "" && path == "" {
		return
	}
	providers := map[string]string{}
	if raw, ok := payload["provider_ids"].(map[string]any); ok {
		for k, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				providers[k] = s
			}
		}
	}
	kind, _ := payload["media_kind"].(string)
	if kind == "" {
		kind, _ = payload["type"].(string)
	}
	title, _ := payload["title"].(string)
	_, _, _ = m.matchAndUpsert(context.Background(), muxID, path, providers, kind, title)
	_ = eventType
}

func discardBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
