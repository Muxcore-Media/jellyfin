package internal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	jellyfinv1 "github.com/Muxcore-Media/jellyfin/proto/jellyfinv1"
)

func (m *Module) ListItemLinks(_ context.Context, req *jellyfinv1.ListItemLinksRequest) (*jellyfinv1.ListItemLinksResponse, error) {
	filter := strings.ToLower(strings.TrimSpace(req.GetMediaKind()))
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*jellyfinv1.ItemLink, 0, len(m.links))
	seen := map[string]bool{}
	for _, link := range m.links {
		if link == nil || link.MuxcoreID == "" {
			continue
		}
		if seen[link.MuxcoreID] {
			continue
		}
		if filter != "" && !strings.EqualFold(link.MediaKind, filter) {
			continue
		}
		seen[link.MuxcoreID] = true
		out = append(out, toProtoLink(link))
	}
	return &jellyfinv1.ListItemLinksResponse{Links: out}, nil
}

func (m *Module) UpsertItemLink(_ context.Context, req *jellyfinv1.UpsertItemLinkRequest) (*jellyfinv1.UpsertItemLinkResponse, error) {
	in := req.GetLink()
	if in == nil || (in.GetMuxcoreId() == "" && in.GetJellyfinId() == "") {
		return nil, fmt.Errorf("muxcore_id or jellyfin_id required")
	}
	link := fromProtoLink(in)
	link.UpdatedAt = time.Now().Unix()
	if err := m.storeLink(link); err != nil {
		return nil, err
	}
	return &jellyfinv1.UpsertItemLinkResponse{Link: toProtoLink(link)}, nil
}

func (m *Module) DeleteItemLink(_ context.Context, req *jellyfinv1.DeleteItemLinkRequest) (*jellyfinv1.DeleteItemLinkResponse, error) {
	muxID := strings.TrimSpace(req.GetMuxcoreId())
	jfID := strings.TrimSpace(req.GetJellyfinId())
	if muxID == "" && jfID == "" {
		return nil, fmt.Errorf("muxcore_id or jellyfin_id required")
	}
	m.mu.Lock()
	if muxID != "" {
		delete(m.links, muxID)
	}
	if jfID != "" {
		delete(m.links, "jf:"+jfID)
		for k, link := range m.links {
			if link != nil && link.JellyfinID == jfID {
				delete(m.links, k)
			}
		}
	}
	m.mu.Unlock()
	if err := m.persistDurable(); err != nil {
		return nil, err
	}
	return &jellyfinv1.DeleteItemLinkResponse{Ok: true}, nil
}

func (m *Module) MatchItem(ctx context.Context, req *jellyfinv1.MatchItemRequest) (*jellyfinv1.MatchItemResponse, error) {
	link, reason, err := m.matchAndUpsert(ctx, req.GetMuxcoreId(), req.GetPath(), req.GetProviderIds(), req.GetMediaKind(), req.GetTitle())
	if err != nil {
		return nil, err
	}
	if link == nil {
		return &jellyfinv1.MatchItemResponse{Matched: false}, nil
	}
	return &jellyfinv1.MatchItemResponse{Link: toProtoLink(link), Matched: true, MatchReason: reason}, nil
}

func (m *Module) SyncLibrary(ctx context.Context, req *jellyfinv1.SyncLibraryRequest) (*jellyfinv1.SyncLibraryResponse, error) {
	if !m.configured() {
		// Soft skip for MVP / unconfigured bridge (smoke expects errors[] note, not RPC failure).
		return &jellyfinv1.SyncLibraryResponse{
			Errors: []string{"jellyfin not configured; sync skipped"},
		}, nil
	}
	dir := strings.ToLower(strings.TrimSpace(req.GetDirection()))
	if dir == "" {
		dir = "both"
	}
	dry := req.GetDryRun()
	resp := &jellyfinv1.SyncLibraryResponse{}

	items, err := m.listJellyfinItems(ctx)
	if err != nil {
		return nil, err
	}
	resp.Scanned = int32(len(items))

	m.mu.RLock()
	mode := m.conflictMode
	existing := make([]*ItemLink, 0, len(m.links))
	seenMux := map[string]bool{}
	for _, link := range m.links {
		if link == nil || link.MuxcoreID == "" || seenMux[link.MuxcoreID] {
			continue
		}
		seenMux[link.MuxcoreID] = true
		cp := *link
		existing = append(existing, &cp)
	}
	m.mu.RUnlock()

	jfByID := map[string]jfItem{}
	jfByPath := map[string]jfItem{}
	jfByProvider := map[string]jfItem{}
	for _, it := range items {
		jfByID[it.ID] = it
		if it.Path != "" {
			jfByPath[normalizePath(it.Path)] = it
		}
		for k, v := range it.ProviderIds {
			if v == "" {
				continue
			}
			jfByProvider[providerKey(k, v)] = it
		}
	}

	if dir == "jellyfin" || dir == "both" || dir == "muxcore" {
		for _, it := range items {
			reason := ""
			var link *ItemLink
			m.mu.RLock()
			if existing := m.links["jf:"+it.ID]; existing != nil {
				cp := *existing
				link = &cp
				reason = "existing"
			}
			m.mu.RUnlock()
			if link == nil {
				for _, ex := range existing {
					if matchProviders(ex.ProviderIDs, it.ProviderIds) {
						cp := *ex
						link = &cp
						reason = "provider_id"
						break
					}
					if ex.Path != "" && normalizePath(ex.Path) == normalizePath(it.Path) {
						cp := *ex
						link = &cp
						reason = "path"
						break
					}
				}
			}
			if link == nil {
				link = &ItemLink{
					MuxcoreID:   "jf:" + it.ID,
					JellyfinID:  it.ID,
					Path:        it.Path,
					ProviderIDs: it.ProviderIds,
					MediaKind:   mediaKindFromJF(it.Type),
					Title:       it.Name,
				}
				reason = "new"
			} else {
				link.JellyfinID = it.ID
				if mode == conflictJellyfin || link.Path == "" {
					link.Path = it.Path
				}
				if mode == conflictJellyfin || link.Title == "" {
					link.Title = it.Name
				}
				if mode == conflictJellyfin || len(link.ProviderIDs) == 0 {
					link.ProviderIDs = it.ProviderIds
				}
				if link.MediaKind == "" {
					link.MediaKind = mediaKindFromJF(it.Type)
				}
			}
			_ = reason
			resp.Matched++
			if !dry {
				link.UpdatedAt = time.Now().Unix()
				if err := m.storeLink(link); err != nil {
					resp.Errors = append(resp.Errors, err.Error())
					continue
				}
				resp.Upserted++
			}
		}
	}

	if dir == "muxcore" || dir == "both" {
		for _, ex := range existing {
			if ex.JellyfinID != "" {
				if _, ok := jfByID[ex.JellyfinID]; ok {
					continue
				}
			}
			matched := false
			var it jfItem
			if ex.Path != "" {
				if found, ok := jfByPath[normalizePath(ex.Path)]; ok {
					it, matched = found, true
				}
			}
			if !matched {
				for k, v := range ex.ProviderIDs {
					if found, ok := jfByProvider[providerKey(k, v)]; ok {
						it, matched = found, true
						break
					}
				}
			}
			if !matched {
				continue
			}
			resp.Matched++
			ex.JellyfinID = it.ID
			if mode != conflictMuxcore {
				if it.Path != "" {
					ex.Path = it.Path
				}
				if it.Name != "" {
					ex.Title = it.Name
				}
				if len(it.ProviderIds) > 0 {
					ex.ProviderIDs = it.ProviderIds
				}
			}
			if !dry {
				ex.UpdatedAt = time.Now().Unix()
				if err := m.storeLink(ex); err != nil {
					resp.Errors = append(resp.Errors, err.Error())
					continue
				}
				resp.Upserted++
			}
		}
	}

	return resp, nil
}

func (m *Module) matchAndUpsert(ctx context.Context, muxID, path string, providers map[string]string, kind, title string) (*ItemLink, string, error) {
	m.mu.RLock()
	if muxID != "" {
		if link := m.links[muxID]; link != nil && link.JellyfinID != "" {
			cp := *link
			m.mu.RUnlock()
			return &cp, "existing", nil
		}
	}
	m.mu.RUnlock()

	if !m.configured() {
		if muxID == "" {
			return nil, "", nil
		}
		link := &ItemLink{
			MuxcoreID:   muxID,
			Path:        path,
			ProviderIDs: providers,
			MediaKind:   kind,
			Title:       title,
			UpdatedAt:   time.Now().Unix(),
		}
		if err := m.storeLink(link); err != nil {
			return nil, "", err
		}
		return link, "existing", nil
	}

	items, err := m.listJellyfinItems(ctx)
	if err != nil {
		return nil, "", err
	}
	var matched *jfItem
	reason := ""
	normPath := normalizePath(path)
	for i := range items {
		it := &items[i]
		if matchProviders(providers, it.ProviderIds) {
			matched = it
			reason = "provider_id"
			break
		}
	}
	if matched == nil && normPath != "" {
		for i := range items {
			it := &items[i]
			if normalizePath(it.Path) == normPath {
				matched = it
				reason = "path"
				break
			}
		}
	}
	if matched == nil {
		if muxID == "" {
			return nil, "", nil
		}
		link := &ItemLink{
			MuxcoreID:   muxID,
			Path:        path,
			ProviderIDs: providers,
			MediaKind:   kind,
			Title:       title,
			UpdatedAt:   time.Now().Unix(),
		}
		if err := m.storeLink(link); err != nil {
			return nil, "", err
		}
		return link, "", nil
	}
	if muxID == "" {
		muxID = "jf:" + matched.ID
	}
	link := &ItemLink{
		MuxcoreID:   muxID,
		JellyfinID:  matched.ID,
		Path:        firstNonEmpty(path, matched.Path),
		ProviderIDs: mergeProviders(providers, matched.ProviderIds),
		MediaKind:   firstNonEmpty(kind, mediaKindFromJF(matched.Type)),
		Title:       firstNonEmpty(title, matched.Name),
		UpdatedAt:   time.Now().Unix(),
	}
	if err := m.storeLink(link); err != nil {
		return nil, "", err
	}
	return link, reason, nil
}

func (m *Module) storeLink(link *ItemLink) error {
	if link == nil {
		return fmt.Errorf("nil link")
	}
	m.mu.Lock()
	if link.MuxcoreID != "" {
		m.links[link.MuxcoreID] = link
	}
	if link.JellyfinID != "" {
		m.links["jf:"+link.JellyfinID] = link
	}
	m.mu.Unlock()
	return m.persistDurable()
}

func toProtoLink(link *ItemLink) *jellyfinv1.ItemLink {
	if link == nil {
		return nil
	}
	return &jellyfinv1.ItemLink{
		MuxcoreId:     link.MuxcoreID,
		JellyfinId:    link.JellyfinID,
		Path:          link.Path,
		ProviderIds:   link.ProviderIDs,
		MediaKind:     link.MediaKind,
		Title:         link.Title,
		UpdatedAtUnix: link.UpdatedAt,
	}
}

func fromProtoLink(in *jellyfinv1.ItemLink) *ItemLink {
	return &ItemLink{
		MuxcoreID:   in.GetMuxcoreId(),
		JellyfinID:  in.GetJellyfinId(),
		Path:        in.GetPath(),
		ProviderIDs: in.GetProviderIds(),
		MediaKind:   in.GetMediaKind(),
		Title:       in.GetTitle(),
		UpdatedAt:   in.GetUpdatedAtUnix(),
	}
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(p))
}

func providerKey(k, v string) string {
	return strings.ToLower(strings.TrimSpace(k)) + "=" + strings.TrimSpace(v)
}

func matchProviders(a, b map[string]string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	bn := map[string]string{}
	for k, v := range b {
		bn[strings.ToLower(k)] = v
	}
	for k, v := range a {
		if v == "" {
			continue
		}
		if bv, ok := bn[strings.ToLower(k)]; ok && bv == v {
			return true
		}
	}
	return false
}

func mergeProviders(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range b {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range a {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
