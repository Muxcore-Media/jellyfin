package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (m *Module) jellyfinGET(ctx context.Context, path string) ([]byte, int, error) {
	m.mu.RLock()
	base, key := m.baseURL, m.apiKey
	m.mu.RUnlock()
	if base == "" || key == "" {
		return nil, 0, fmt.Errorf("jellyfin not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, key))
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpCli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (m *Module) jellyfinPOST(ctx context.Context, path string) error {
	m.mu.RLock()
	base, key := m.baseURL, m.apiKey
	m.mu.RUnlock()
	if base == "" || key == "" {
		return fmt.Errorf("jellyfin not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, key))
	resp, err := m.httpCli.Do(req)
	if err != nil {
		return err
	}
	discardBody(resp)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin POST %s status %d", path, resp.StatusCode)
	}
	return nil
}

func (m *Module) refreshLibrary(ctx context.Context, itemID string) error {
	endpoint := "/Library/Refresh"
	if itemID != "" {
		endpoint = fmt.Sprintf("/Items/%s/Refresh?Recursive=true", url.PathEscape(itemID))
	}
	return m.jellyfinPOST(ctx, endpoint)
}

func (m *Module) probeSystemInfo(ctx context.Context) error {
	body, code, err := m.jellyfinGET(ctx, "/System/Info")
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("jellyfin /System/Info status %d", code)
	}
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("jellyfin /System/Info parse: %w", err)
	}
	return nil
}

type jfItem struct {
	ID             string            `json:"Id"`
	Name           string            `json:"Name"`
	Type           string            `json:"Type"`
	Path           string            `json:"Path"`
	Size           int64             `json:"Size"`
	ParentId       string            `json:"ParentId"`
	Width          int               `json:"Width"`
	Height         int               `json:"Height"`
	ProviderIds    map[string]string `json:"ProviderIds"`
	ProductionYear int               `json:"ProductionYear"`
}

type jfVirtualFolder struct {
	Name string `json:"Name"`
	Id   string `json:"Id"`
}

type jfItemsResponse struct {
	Items []jfItem `json:"Items"`
}

func (m *Module) listJellyfinItems(ctx context.Context) ([]jfItem, error) {
	q := url.Values{}
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Movie,Series,Episode")
	q.Set("Fields", "Path,ProviderIds,Size,ParentId,Width,Height")
	q.Set("EnableTotalRecordCount", "false")
	body, code, err := m.jellyfinGET(ctx, "/Items?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("jellyfin /Items status %d", code)
	}
	var raw jfItemsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return raw.Items, nil
}

func (m *Module) listJellyfinVirtualFolders(ctx context.Context) ([]jfVirtualFolder, error) {
	body, code, err := m.jellyfinGET(ctx, "/Library/VirtualFolders")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("jellyfin /Library/VirtualFolders status %d", code)
	}
	var folders []jfVirtualFolder
	if err := json.Unmarshal(body, &folders); err != nil {
		return nil, err
	}
	return folders, nil
}

type jfSession struct {
	Id             string  `json:"Id"`
	UserId         string  `json:"UserId"`
	UserName       string  `json:"UserName"`
	Client         string  `json:"Client"`
	DeviceName     string  `json:"DeviceName"`
	RemoteEndPoint string  `json:"RemoteEndPoint"`
	AppName        string  `json:"AppName"`
	NowPlayingItem *jfItem `json:"NowPlayingItem"`
	PlayState      *struct {
		PositionTicks int64  `json:"PositionTicks"`
		IsPaused      bool   `json:"IsPaused"`
		PlayMethod    string `json:"PlayMethod"`
	} `json:"PlayState"`
	TranscodingInfo *struct {
		CompletionPercentage float64 `json:"CompletionPercentage"`
		Width                int     `json:"Width"`
		Height               int     `json:"Height"`
	} `json:"TranscodingInfo"`
}

func (m *Module) listSessions(ctx context.Context) ([]jfSession, error) {
	body, code, err := m.jellyfinGET(ctx, "/Sessions")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("jellyfin /Sessions status %d", code)
	}
	var sessions []jfSession
	if err := json.Unmarshal(body, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func mediaKindFromJF(t string) string {
	switch strings.ToLower(t) {
	case "movie":
		return "movie"
	case "series":
		return "tv"
	case "episode":
		return "episode"
	default:
		return "other"
	}
}
