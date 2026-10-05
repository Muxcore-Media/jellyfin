package internal

import (
	"errors"
	"testing"

	"github.com/Muxcore-Media/core/sdk/go/module/netguard"
)

func TestGuardOutboundURL(t *testing.T) {
	if err := guardOutboundURL(""); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"http://127.0.0.1:8096/System/Info",
		"http://192.168.1.20:8096/Items",
		"http://jellyfin:8096/api/sse/events",
		"http://userdata-local:9680/userdata",
	} {
		if err := guardOutboundURL(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/",
		"file:///etc/passwd",
	} {
		if err := guardOutboundURL(raw); !errors.Is(err, netguard.ErrBlocked) {
			t.Fatalf("%s: err=%v", raw, err)
		}
	}
}

func TestUpdateSettingRejectsMetadataBaseURL(t *testing.T) {
	m := NewModule(Config{DataDir: t.TempDir()})
	err := m.updateSetting("base_url", "http://169.254.169.254")
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("err=%v", err)
	}
	if m.baseURL != "" {
		t.Fatalf("stored %q", m.baseURL)
	}
	if err := m.updateSetting("base_url", "http://127.0.0.1:8096"); err != nil {
		t.Fatal(err)
	}
}
