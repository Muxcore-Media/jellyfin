package internal

import (
	"context"
	"encoding/json"
	"strings"

	playbackv1 "github.com/Muxcore-Media/playback-contract/proto/playbackv1"
)

type libraryCatalogPayload struct {
	Action        string `json:"action"`
	ServerID      string `json:"server_id"`
	ServerType    string `json:"server_type"`
	ItemID        string `json:"item_id"`
	MediaType     string `json:"media_type,omitempty"`
	ParentID      string `json:"parent_id,omitempty"`
	MuxcoreID     string `json:"muxcore_id,omitempty"`
	Title         string `json:"title,omitempty"`
	MediaPath     string `json:"media_path,omitempty"`
	FileSizeBytes int64  `json:"file_size_bytes,omitempty"`
	VideoResolution string `json:"video_resolution,omitempty"`
}

func (m *Module) publishLibraryCatalogEvent(ctx context.Context, action string, raw map[string]any) {
	itemID := stringField(raw, "itemId", "item_id", "ItemId")
	if itemID == "" {
		return
	}
	mediaType := stringField(raw, "itemType", "item_type", "ItemType", "Type")
	title := stringField(raw, "title", "Title", "Name")
	mediaPath := stringField(raw, "path", "Path", "mediaPath")
	fileSize := int64Field(raw, "size", "Size", "fileSize", "FileSize")
	height := int(int64Field(raw, "height", "Height", "video_height", "videoHeight"))
	width := int(int64Field(raw, "width", "Width", "video_width", "videoWidth"))
	videoResolution := playbackv1.NormalizeStreamResolution(height, width, stringField(raw, "video_resolution", "videoResolution"))
	payload, err := json.Marshal(libraryCatalogPayload{
		Action:        action,
		ServerID:      m.id,
		ServerType:    "jellyfin",
		ItemID:        itemID,
		MediaType:     mediaType,
		ParentID:      stringField(raw, "parentId", "parent_id", "ParentId"),
		MuxcoreID:     m.muxcoreIDForJellyfin(itemID),
		Title:         title,
		MediaPath:     mediaPath,
		FileSizeBytes: fileSize,
		VideoResolution: videoResolution,
	})
	if err != nil {
		return
	}
	if err := m.publishEvent(ctx, "playback.library.item", payload); err != nil {
		// mesh optional in tests
		_ = err
	}
}

func (m *Module) handlePluginLibrarySSE(eventName, data string) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &raw); err != nil {
		return
	}
	action := "added"
	if strings.Contains(strings.ToLower(eventName), "removed") {
		action = "removed"
	}
	m.publishLibraryCatalogEvent(context.Background(), action, raw)
}
