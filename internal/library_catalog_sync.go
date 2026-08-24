package internal

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	playbackv1 "github.com/Muxcore-Media/playback-contract/proto/playbackv1"
)

func (m *Module) jellyfinLibraryNameMap(ctx context.Context) map[string]string {
	out := map[string]string{}
	folders, err := m.listJellyfinVirtualFolders(ctx)
	if err != nil {
		return out
	}
	for _, f := range folders {
		if f.Id != "" && f.Name != "" {
			out[f.Id] = f.Name
		}
	}
	return out
}

func (m *Module) publishCatalogFromJFItem(ctx context.Context, libNames map[string]string, it jfItem) {
	if it.ID == "" {
		return
	}
	libraryName := libNames[it.ParentId]
	m.publishLibraryCatalogEvent(ctx, "upsert", map[string]any{
		"itemId":           it.ID,
		"itemType":         it.Type,
		"title":            it.Name,
		"path":             it.Path,
		"size":             it.Size,
		"parentId":         it.ParentId,
		"library_name":     libraryName,
		"libraryName":      libraryName,
		"file_size_bytes":  it.Size,
		"height":           it.Height,
		"width":            it.Width,
		"video_resolution": playbackv1.NormalizeStreamResolution(it.Height, it.Width, ""),
	})
}

func (m *Module) syncLibraryCatalog(ctx context.Context) (int, error) {
	if !m.configured() {
		return 0, nil
	}
	items, err := m.listJellyfinItems(ctx)
	if err != nil {
		return 0, err
	}
	libNames := m.jellyfinLibraryNameMap(ctx)
	published := 0
	for _, it := range items {
		m.publishCatalogFromJFItem(ctx, libNames, it)
		published++
	}
	return published, nil
}

func (m *Module) catalogSyncIntervalSec() int {
	if v := os.Getenv("JELLYFIN_CATALOG_SYNC_SEC"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return 6 * 3600
}

func (m *Module) catalogSyncLoop() {
	interval := time.Duration(m.catalogSyncIntervalSec()) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	run := func() {
		if !m.configured() {
			return
		}
		n, err := m.syncLibraryCatalog(context.Background())
		if err != nil {
			slog.Debug("jellyfin: library catalog sync failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("jellyfin: library catalog synced", "items", n)
		}
	}
	run()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			run()
		}
	}
}
