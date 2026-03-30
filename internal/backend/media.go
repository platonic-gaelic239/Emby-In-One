package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type routeResolution struct {
	OriginalID     string
	ServerIndex    int
	Client         *UpstreamClient
	OtherInstances []AdditionalInstance
}

type upstreamItemsResult struct {
	ServerIndex int
	Items       []map[string]any
}

func (a *App) registerMediaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /Users/{userId}/Items", a.withContext(a.requireAuth(a.handleUserItems)))
	mux.HandleFunc("GET /Items", a.withContext(a.requireAuth(a.handleItemsCollection)))
	mux.HandleFunc("GET /Users/{userId}/Items/Resume", a.withContext(a.requireAuth(a.handleUserItemsResume)))
	mux.HandleFunc("GET /Users/{userId}/Items/Latest", a.withContext(a.requireAuth(a.handleUserItemsLatest)))
	mux.HandleFunc("GET /Users/{userId}/Items/{itemId}", a.withContext(a.requireAuth(a.handleUserItemByID)))
	mux.HandleFunc("GET /Items/{itemId}", a.withContext(a.requireAuth(a.handleItemByID)))
	mux.HandleFunc("GET /Items/{itemId}/Similar", a.withContext(a.requireAuth(a.handleItemSimilar)))
	mux.HandleFunc("GET /Items/{itemId}/ThemeMedia", a.withContext(a.requireAuth(a.handleItemThemeMedia)))
	mux.HandleFunc("GET /Shows/NextUp", a.withContext(a.requireAuth(a.handleShowsNextUp)))
	mux.HandleFunc("GET /Items/{itemId}/PlaybackInfo", a.withContext(a.requireAuth(a.handlePlaybackInfo)))
	mux.HandleFunc("POST /Items/{itemId}/PlaybackInfo", a.withContext(a.requireAuth(a.handlePlaybackInfo)))
	mux.HandleFunc("GET /Videos/{itemId}/{rest...}", a.withContext(a.requireAuth(a.handleVideoProxy)))
	mux.HandleFunc("GET /Audio/{itemId}/{rest...}", a.withContext(a.requireAuth(a.handleAudioProxy)))
	mux.HandleFunc("DELETE /Videos/ActiveEncodings", a.withContext(a.requireAuth(a.handleDeleteActiveEncodings)))
}

func (a *App) resolveRouteID(id string) *routeResolution {
	resolved := a.IDStore.ResolveVirtualID(id)
	if resolved == nil {
		return nil
	}
	client := a.Upstream.GetClient(resolved.ServerIndex)
	if client == nil || !client.IsOnline() {
		return nil
	}
	return &routeResolution{
		OriginalID:     resolved.OriginalID,
		ServerIndex:    resolved.ServerIndex,
		Client:         client,
		OtherInstances: append([]AdditionalInstance(nil), resolved.OtherInstances...),
	}
}

func cloneValues(values url.Values) url.Values {
	cloned := url.Values{}
	for key, rawValues := range values {
		cloned[key] = append([]string(nil), rawValues...)
	}
	return cloned
}

func asItems(payload any) []map[string]any {
	switch typed := payload.(type) {
	case []any:
		items := make([]map[string]any, 0, len(typed))
		for _, raw := range typed {
			if item, ok := raw.(map[string]any); ok {
				items = append(items, item)
			}
		}
		return items
	case map[string]any:
		if rawItems, ok := typed["Items"]; ok {
			return asItems(rawItems)
		}
		if rawItems, ok := typed["items"]; ok {
			return asItems(rawItems)
		}
	}
	return []map[string]any{}
}

func (a *App) rewriteItems(items []map[string]any, serverIndex int) []map[string]any {
	cfg := a.ConfigStore.Snapshot()
	for _, item := range items {
		rewriteResponseIDs(item, serverIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	}
	return items
}

func (a *App) handleItemsCollection(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	if !hasBatchIDQuery(query) {
		a.handleFallbackProxy(w, r)
		return
	}
	clients := a.Upstream.OnlineClients()
	type slot struct {
		result upstreamItemsResult
		ok     bool
	}
	slots := make([]slot, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *UpstreamClient) {
			defer wg.Done()
			serverQuery, ok := translateBatchIDQueryForServer(query, c.ServerIndex, a.IDStore)
			if !ok {
				return
			}
			payload, err := c.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Items", serverQuery, nil)
			if err != nil {
				return
			}
			slots[idx] = slot{result: upstreamItemsResult{ServerIndex: c.ServerIndex, Items: asItems(payload)}, ok: true}
		}(i, client)
	}
	wg.Wait()
	results := make([]upstreamItemsResult, 0, len(clients))
	for _, s := range slots {
		if s.ok {
			results = append(results, s.result)
		}
	}
	writeJSON(w, http.StatusOK, a.mergedItemsPayload(results))
}

func (a *App) handleUserItems(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" && parentID != "0" && parentID != "root" {
		resolved := a.resolveRouteID(parentID)
		if resolved == nil {
			writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		query.Set("ParentId", resolved.OriginalID)
		query.Del("parentId")
		query.Del("parentid")
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.UserID+"/Items", query, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
			return
		}
		a.rewriteItems(asItems(payload), resolved.ServerIndex)
		writeJSON(w, http.StatusOK, payload)
		return
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Users/%s/Items", query, nil)
	merged := a.mergedItemsPayload(results)
	if items, ok := merged["Items"].([]any); ok {
		asMaps := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				asMaps = append(asMaps, m)
			}
		}
		writeJSON(w, http.StatusOK, paginateItems(asMaps, r.URL.Query()))
		return
	}
	writeJSON(w, http.StatusOK, merged)
}

func (a *App) handleUserItemsResume(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" {
		resolved := a.resolveRouteID(parentID)
		if resolved == nil {
			writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		instances := buildSeriesInstances(resolved, a.Upstream)
		originalIDs := map[string]struct{}{}
		for _, inst := range instances {
			originalIDs[inst.OriginalID] = struct{}{}
		}
		for _, inst := range instances {
			instQuery := cloneValues(query)
			instQuery.Set("ParentId", inst.OriginalID)
			instQuery.Del("parentId")
			instQuery.Del("parentid")
			payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+inst.Client.UserID+"/Items/Resume", instQuery, nil)
			if err != nil {
				continue
			}
			filtered := filterSeriesItems(asItems(payload), originalIDs)
			if len(filtered) > 0 {
				a.rewriteItems(filtered, inst.ServerIndex)
				writeJSON(w, http.StatusOK, map[string]any{"Items": filtered, "TotalRecordCount": len(filtered), "StartIndex": 0})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Users/%s/Items/Resume", query, nil)
	writeJSON(w, http.StatusOK, a.mergedItemsPayload(results))
}

func (a *App) handleUserItemsLatest(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	parentID := query.Get("ParentId")
	if parentID != "" {
		resolved := a.resolveRouteID(parentID)
		if resolved == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		query.Set("ParentId", resolved.OriginalID)
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.UserID+"/Items/Latest", query, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
			return
		}
		items := asItems(payload)
		a.rewriteItems(items, resolved.ServerIndex)
		writeJSON(w, http.StatusOK, items)
		return
	}
	clients := a.Upstream.OnlineClients()
	type slot struct {
		items []map[string]any
	}
	slots := make([]slot, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *UpstreamClient) {
			defer wg.Done()
			payload, err := c.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+c.UserID+"/Items/Latest", query, nil)
			if err != nil {
				return
			}
			items := asItems(payload)
			a.rewriteItems(items, c.ServerIndex)
			slots[idx] = slot{items: items}
		}(i, client)
	}
	wg.Wait()
	allItems := make([]map[string]any, 0)
	for _, s := range slots {
		allItems = append(allItems, s.items...)
	}
	writeJSON(w, http.StatusOK, allItems)
}
func (a *App) handleShowsNextUp(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	seriesID := query.Get("SeriesId")
	if seriesID != "" {
		resolved := a.resolveRouteID(seriesID)
		if resolved == nil {
			writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		instances := buildSeriesInstances(resolved, a.Upstream)
		originalIDs := map[string]struct{}{}
		for _, inst := range instances {
			originalIDs[inst.OriginalID] = struct{}{}
		}
		for _, inst := range instances {
			instQuery := cloneValues(query)
			instQuery.Set("SeriesId", inst.OriginalID)
			payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Shows/NextUp", instQuery, nil)
			if err != nil {
				continue
			}
			filtered := filterSeriesItems(asItems(payload), originalIDs)
			if len(filtered) > 0 {
				a.rewriteItems(filtered, inst.ServerIndex)
				writeJSON(w, http.StatusOK, map[string]any{"Items": filtered, "TotalRecordCount": len(filtered), "StartIndex": 0})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Shows/NextUp", query, nil)
	writeJSON(w, http.StatusOK, a.mergedItemsPayload(results))
}

func (a *App) handleUserItemByID(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	instances := a.collectItemInstances(resolved)
	if len(instances) <= 1 {
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.UserID+"/Items/"+resolved.OriginalID, cloneValues(r.URL.Query()), nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
			return
		}
		cfg := a.ConfigStore.Snapshot()
		rewriteResponseIDs(payload, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
		writeJSON(w, http.StatusOK, payload)
		return
	}

	var base map[string]any
	allMediaSources := []map[string]any{}
	for _, inst := range instances {
		payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+inst.Client.UserID+"/Items/"+inst.OriginalID, cloneValues(r.URL.Query()), nil)
		if err != nil {
			continue
		}
		data, ok := payload.(map[string]any)
		if !ok {
			continue
		}
		if base == nil {
			base = deepCloneMap(data)
		}
		for _, raw := range asItems(map[string]any{"Items": data["MediaSources"]}) {
			mediaSource := deepCloneMap(raw)
			if originalID, _ := mediaSource["Id"].(string); originalID != "" {
				mediaSource["Id"] = a.IDStore.GetOrCreateVirtualID(originalID, inst.ServerIndex)
			}
			if client := a.Upstream.GetClient(inst.ServerIndex); client != nil {
				name, _ := mediaSource["Name"].(string)
				if name == "" {
					name = "Version"
				}
				mediaSource["Name"] = name + " [" + client.Name + "]"
			}
			allMediaSources = append(allMediaSources, mediaSource)
		}
	}
	if base == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream request failed"})
		return
	}
	mediaSources := make([]any, 0, len(allMediaSources))
	for _, mediaSource := range allMediaSources {
		mediaSources = append(mediaSources, mediaSource)
	}
	cfg := a.ConfigStore.Snapshot()
	// delete-and-restore: prevent rewriteResponseIDs from double-wrapping
	// already-virtualised MediaSource IDs and creating orphan mappings
	delete(base, "MediaSources")
	rewriteResponseIDs(base, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	base["MediaSources"] = mediaSources
	writeJSON(w, http.StatusOK, base)
}

func (a *App) handleItemByID(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Items/"+resolved.OriginalID, cloneValues(r.URL.Query()), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	writeJSON(w, http.StatusOK, payload)
}

func (a *App) handleItemSimilar(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		return
	}
	query := cloneValues(r.URL.Query())
	query.Set("UserId", resolved.Client.UserID)
	payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Items/"+resolved.OriginalID+"/Similar", query, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	writeJSON(w, http.StatusOK, payload)
}

func (a *App) handleItemThemeMedia(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ThemeVideosResult":     map[string]any{"Items": []any{}, "TotalRecordCount": 0},
			"ThemeSongsResult":      map[string]any{"Items": []any{}, "TotalRecordCount": 0},
			"SoundtrackSongsResult": map[string]any{"Items": []any{}, "TotalRecordCount": 0},
		})
		return
	}
	query := cloneValues(r.URL.Query())
	query.Set("UserId", resolved.Client.UserID)
	payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Items/"+resolved.OriginalID+"/ThemeMedia", query, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	writeJSON(w, http.StatusOK, payload)
}
func (a *App) fetchItemsAcrossUpstreams(ctx context.Context, reqCtx *RequestContext, pathTemplate string, query url.Values, body any) []upstreamItemsResult {
	clients := a.Upstream.OnlineClients()
	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, globalTimeout)
	defer cancel()
	type slot struct {
		result upstreamItemsResult
		ok     bool
	}
	slots := make([]slot, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *UpstreamClient) {
			defer wg.Done()
			serverQuery := cloneValues(query)
			if hasBatchIDQuery(serverQuery) {
				translated, ok := translateBatchIDQueryForServer(serverQuery, c.ServerIndex, a.IDStore)
				if !ok {
					return
				}
				serverQuery = translated
			}
			payload, err := c.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet, strings.Replace(pathTemplate, "%s", c.UserID, 1), serverQuery, body)
			if err != nil {
				return
			}
			slots[idx] = slot{result: upstreamItemsResult{ServerIndex: c.ServerIndex, Items: asItems(payload)}, ok: true}
		}(i, client)
	}
	wg.Wait()
	results := make([]upstreamItemsResult, 0, len(clients))
	for _, s := range slots {
		if s.ok {
			results = append(results, s.result)
		}
	}
	return results
}

// getItemKey generates a deduplication key for an item.
// Returns empty string if the item cannot be deduplicated.
func getItemKey(item map[string]any) string {
	// Priority 1: TMDB ID
	if providerIDs, ok := item["ProviderIds"].(map[string]any); ok {
		if tmdb, ok := providerIDs["Tmdb"].(string); ok && tmdb != "" {
			return "tmdb:" + tmdb
		}
	}
	// Priority 2: Movie/Series by Name + Year
	itemType, _ := item["Type"].(string)
	if itemType == "Movie" || itemType == "Series" {
		name, _ := item["Name"].(string)
		year := ""
		if y, ok := numericInt(item["ProductionYear"]); ok {
			year = strconv.Itoa(y)
		}
		return "name:" + strings.ToLower(name) + ":" + year
	}
	// Priority 3: Episode by SeriesName + Season:Episode
	if itemType == "Episode" {
		seriesName, _ := item["SeriesName"].(string)
		parentIdx, okP := numericInt(item["ParentIndexNumber"])
		idx, okE := numericInt(item["IndexNumber"])
		if seriesName != "" && okP && okE {
			return "ep:" + strings.ToLower(seriesName) + ":S" + strconv.Itoa(parentIdx) + "E" + strconv.Itoa(idx)
		}
	}
	return ""
}

// containsChinese checks if a string contains CJK Unified Ideographs (U+4E00–U+9FA5).
func containsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FA5 {
			return true
		}
	}
	return false
}

// isBetterMetadata returns true if the candidate item from candidateIdx has
// better metadata than the existing item from existingIdx, using the V1.2
// 4-level priority: priorityMetadata flag → Chinese in Overview → longer
// Overview → lower server index.
func isBetterMetadata(existing map[string]any, existingIdx int, candidate map[string]any, candidateIdx int, cfg Config) bool {
	// 1. priorityMetadata flag
	existingPriority := false
	candidatePriority := false
	if existingIdx >= 0 && existingIdx < len(cfg.Upstream) {
		existingPriority = cfg.Upstream[existingIdx].PriorityMetadata
	}
	if candidateIdx >= 0 && candidateIdx < len(cfg.Upstream) {
		candidatePriority = cfg.Upstream[candidateIdx].PriorityMetadata
	}
	if candidatePriority && !existingPriority {
		return true
	}
	if existingPriority && !candidatePriority {
		return false
	}

	// 2. Chinese in Overview
	existingOverview, _ := existing["Overview"].(string)
	candidateOverview, _ := candidate["Overview"].(string)
	hasChinese1 := containsChinese(existingOverview)
	hasChinese2 := containsChinese(candidateOverview)
	if hasChinese2 && !hasChinese1 {
		return true
	}
	if hasChinese1 && !hasChinese2 {
		return false
	}

	// 3. Longer Overview
	if len(candidateOverview) > len(existingOverview) {
		return true
	}
	if len(existingOverview) > len(candidateOverview) {
		return false
	}

	// 4. Lower server index
	return candidateIdx < existingIdx
}

func (a *App) mergedItemsPayload(results []upstreamItemsResult) map[string]any {
	if len(results) == 0 {
		return map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0}
	}

	cfg := a.ConfigStore.Snapshot()
	merged := make([]map[string]any, 0)
	type seenEntry struct {
		virtualID   string
		mergedIndex int // position in merged slice
		serverIndex int
	}
	seen := map[string]*seenEntry{} // dedupKey → entry

	// Interleaved round-robin: take one item from each server in turn
	maxLen := 0
	for _, r := range results {
		if len(r.Items) > maxLen {
			maxLen = len(r.Items)
		}
	}

	for i := 0; i < maxLen; i++ {
		for _, r := range results {
			if i >= len(r.Items) {
				continue
			}
			item := r.Items[i]
			key := getItemKey(item)

			if key != "" {
				if entry, found := seen[key]; found {
					// Duplicate: associate as additional instance
					if originalID, ok := item["Id"].(string); ok && originalID != "" {
						a.IDStore.AssociateAdditionalInstance(entry.virtualID, originalID, r.ServerIndex)
					}
					// Check if candidate has better metadata; if so, replace display item
					if isBetterMetadata(merged[entry.mergedIndex], entry.serverIndex, item, r.ServerIndex, cfg) {
						delete(item, "Id")
						rewriteResponseIDs(item, r.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
						item["Id"] = entry.virtualID
						merged[entry.mergedIndex] = item
						entry.serverIndex = r.ServerIndex
					}
					continue
				}
				// First occurrence: create virtual ID, rewrite other fields without touching Id
				if originalID, ok := item["Id"].(string); ok && originalID != "" {
					virtualID := a.IDStore.GetOrCreateVirtualID(originalID, r.ServerIndex)
					seen[key] = &seenEntry{virtualID: virtualID, mergedIndex: len(merged), serverIndex: r.ServerIndex}
					delete(item, "Id")
					rewriteResponseIDs(item, r.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
					item["Id"] = virtualID
					merged = append(merged, item)
					continue
				}
			}

			// Rewrite IDs and add to result
			rewriteResponseIDs(item, r.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
			merged = append(merged, item)
		}
	}

	return map[string]any{
		"Items":            toAnySlice(merged),
		"TotalRecordCount": len(merged),
		"StartIndex":       0,
	}
}

func paginateItems(merged []map[string]any, query url.Values) map[string]any {
	totalCount := len(merged)
	startIndex := 0
	if s := query.Get("StartIndex"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			startIndex = v
		}
	}
	if startIndex > len(merged) {
		startIndex = len(merged)
	}
	merged = merged[startIndex:]
	if lim := query.Get("Limit"); lim != "" {
		if v, err := strconv.Atoi(lim); err == nil && v >= 0 && v < len(merged) {
			merged = merged[:v]
		}
	}
	return map[string]any{
		"Items":            toAnySlice(merged),
		"TotalRecordCount": totalCount,
		"StartIndex":       startIndex,
	}
}

func (a *App) collectItemInstances(resolved *routeResolution) []seriesInstance {
	return buildSeriesInstances(resolved, a.Upstream)
}

func firstQueryValue(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := values.Get(key); value != "" {
			return value
		}
	}
	return ""
}

func (a *App) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		if a.Logger != nil {
			a.Logger.Warnf("PlaybackInfo: itemId=%s not found in mappings", r.PathValue("itemId"))
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	instances := a.collectItemInstances(resolved)
	if a.Logger != nil {
		a.Logger.Debugf("PlaybackInfo: itemId=%s → server=[%s] originalId=%s, instances=%d",
			r.PathValue("itemId"), resolved.Client.Name, resolved.OriginalID, len(instances))
	}
	query := cloneValues(r.URL.Query())
	body := map[string]any{}
	if r.Method == http.MethodPost && r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if mediaSourceID, ok := body["MediaSourceId"].(string); ok {
		if msResolved := a.IDStore.ResolveVirtualID(mediaSourceID); msResolved != nil {
			body["MediaSourceId"] = msResolved.OriginalID
		}
	}
	if mediaSourceID := query.Get("MediaSourceId"); mediaSourceID != "" {
		if msResolved := a.IDStore.ResolveVirtualID(mediaSourceID); msResolved != nil {
			query.Set("MediaSourceId", msResolved.OriginalID)
		}
	}

	// Remove proxy token from query before forwarding to upstream
	query.Del("api_key")
	query.Del("ApiKey")

	var base map[string]any
	allMediaSources := []map[string]any{}
	for _, inst := range instances {
		payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, r.Method, "/Items/"+inst.OriginalID+"/PlaybackInfo", query, body)
		if err != nil {
			continue
		}
		data, ok := payload.(map[string]any)
		if !ok {
			continue
		}
		if base == nil {
			base = deepCloneMap(data)
		}
		for _, raw := range asItems(map[string]any{"Items": data["MediaSources"]}) {
			mediaSource := deepCloneMap(raw)
			originalMSID, _ := mediaSource["Id"].(string)
			virtualMSID := originalMSID
			if originalMSID != "" {
				virtualMSID = a.IDStore.GetOrCreateVirtualID(originalMSID, inst.ServerIndex)
				mediaSource["Id"] = virtualMSID
			}
			if directURL, ok := mediaSource["DirectStreamUrl"].(string); ok && directURL != "" {
				// Store original full URL for redirect mode
				a.IDStore.SetMediaSourceStreamURL(virtualMSID, resolveAbsoluteStreamURL(inst.Client.StreamBaseURL, directURL))

				// Extract container from URL, stripping query string first
				// Node.js uses regex /\.([a-z0-9]+)(?:\?|$)/i — path.Ext doesn't stop at '?'
				cleanURL := directURL
				if qIdx := strings.IndexByte(cleanURL, '?'); qIdx >= 0 {
					cleanURL = cleanURL[:qIdx]
				}
				container := strings.TrimPrefix(path.Ext(cleanURL), ".")
				if container == "" {
					if rawContainer, ok := mediaSource["Container"].(string); ok {
						container = rawContainer
					}
				}
				if container == "" {
					container = "mp4"
				}
				proxyURL := url.Values{}
				proxyURL.Set("MediaSourceId", virtualMSID)
				proxyURL.Set("Static", "true")
				if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyToken != "" {
					proxyURL.Set("api_key", reqCtx.ProxyToken)
				}
				mediaSource["DirectStreamUrl"] = "/Videos/" + r.PathValue("itemId") + "/stream." + container + "?" + proxyURL.Encode()
			}
			if transcodingURL, ok := mediaSource["TranscodingUrl"].(string); ok && transcodingURL != "" {
				// Store original full URL for redirect mode
				a.IDStore.SetMediaSourceStreamURL(virtualMSID+"_transcode", resolveAbsoluteStreamURL(inst.Client.StreamBaseURL, transcodingURL))

				parsed, err := url.Parse(transcodingURL)
				if err == nil {
					if parsed.Scheme == "" {
						parsed, _ = url.Parse(inst.Client.StreamBaseURL + transcodingURL)
					}
					proxyPath := parsed.Path
					proxyPath = strings.Replace(proxyPath, "/Videos/"+inst.OriginalID+"/", "/Videos/"+r.PathValue("itemId")+"/", 1)
					proxyPath = strings.Replace(proxyPath, "/Audio/"+inst.OriginalID+"/", "/Audio/"+r.PathValue("itemId")+"/", 1)
					queryValues := parsed.Query()
					queryValues.Del("api_key")
					queryValues.Del("ApiKey")
					if originalMSID != "" {
						queryValues.Set("MediaSourceId", virtualMSID)
					}
					if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyToken != "" {
						queryValues.Set("api_key", reqCtx.ProxyToken)
					}
					mediaSource["TranscodingUrl"] = proxyPath + "?" + queryValues.Encode()
				}
			}
			// Rewrite MediaSource.Path for Http protocol and MediaStreams[].DeliveryUrl
			if protocol, _ := mediaSource["Protocol"].(string); protocol == "Http" {
				if msPath, ok := mediaSource["Path"].(string); ok && msPath != "" {
					msPath = strings.ReplaceAll(msPath, inst.OriginalID, r.PathValue("itemId"))
					if originalMSID != "" && originalMSID != inst.OriginalID {
						msPath = strings.ReplaceAll(msPath, originalMSID, virtualMSID)
					}
					mediaSource["Path"] = msPath
				}
			}
			if rawStreams, ok := mediaSource["MediaStreams"].([]any); ok {
				for _, rawStream := range rawStreams {
					if stream, ok := rawStream.(map[string]any); ok {
						if deliveryURL, ok := stream["DeliveryUrl"].(string); ok && deliveryURL != "" {
							deliveryURL = strings.ReplaceAll(deliveryURL, inst.OriginalID, r.PathValue("itemId"))
							if originalMSID != "" && originalMSID != inst.OriginalID {
								deliveryURL = strings.ReplaceAll(deliveryURL, originalMSID, virtualMSID)
							}
							stream["DeliveryUrl"] = deliveryURL
						}
					}
				}
			}
			allMediaSources = append(allMediaSources, mediaSource)
		}
	}
	// Record which server served this virtual item so Sessions/Playing routes back correctly
	if len(allMediaSources) > 0 {
		if virtualItemID := r.PathValue("itemId"); virtualItemID != "" {
			a.IDStore.SetActiveStream(virtualItemID, resolved.ServerIndex)
		}
	}
	if base == nil {
		if a.Logger != nil {
			a.Logger.Errorf("PlaybackInfo: all upstream requests failed for itemId=%s", r.PathValue("itemId"))
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Failed to fetch playback info from upstream"})
		return
	}
	if a.Logger != nil {
		a.Logger.Debugf("PlaybackInfo: returning %d MediaSources for itemId=%s", len(allMediaSources), r.PathValue("itemId"))
	}
	cfg := a.ConfigStore.Snapshot()
	// Rewrite top-level fields (excluding MediaSources which were already virtualised per-server above)
	delete(base, "MediaSources")
	rewriteResponseIDs(base, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	base["MediaSources"] = make([]any, 0, len(allMediaSources))
	for _, mediaSource := range allMediaSources {
		base["MediaSources"] = append(base["MediaSources"].([]any), mediaSource)
	}
	writeJSON(w, http.StatusOK, base)
}

func deepCloneMap(source map[string]any) map[string]any {
	encoded, _ := json.Marshal(source)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	return decoded
}

// resolveAbsoluteStreamURL resolves a possibly-relative stream URL against a base URL.
func resolveAbsoluteStreamURL(base, rawURL string) string {
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		return rawURL
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(rawURL, "/")
}

// resolveMediaSourceInPath resolves a virtual MediaSourceId embedded as the first path
// segment when followed by /Subtitles/ or /Attachments/ (e.g. "{msId}/Subtitles/0/Stream.srt").
func resolveMediaSourceInPath(rest string, idStore *IDStore) string {
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return rest
	}
	firstSeg := rest[:slash]
	after := rest[slash:] // includes leading '/'
	if !strings.HasPrefix(after, "/Subtitles/") && !strings.HasPrefix(after, "/Attachments/") {
		return rest
	}
	if resolved := idStore.ResolveVirtualID(firstSeg); resolved != nil {
		return resolved.OriginalID + after
	}
	return rest
}

func (a *App) handleVideoProxy(w http.ResponseWriter, r *http.Request) {
	virtualItemID := r.PathValue("itemId")
	query := cloneValues(r.URL.Query())
	if a.Logger != nil {
		a.Logger.Debugf("Stream request: itemId=%s, query=%s", virtualItemID, query.Encode())
	}
	resolved := a.resolveRouteID(virtualItemID)
	if resolved == nil {
		if a.Logger != nil {
			a.Logger.Warnf("Stream: itemId=%s not found in mappings", virtualItemID)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	rest := r.PathValue("rest")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Stream not found"})
		return
	}
	// Resolve virtual MediaSourceId in subtitle/attachment paths:
	// /Videos/{itemId}/{mediaSourceId}/Subtitles/...  or  .../Attachments/...
	rest = resolveMediaSourceInPath(rest, a.IDStore)
	actualClient := resolved.Client
	actualOriginalID := resolved.OriginalID

	virtualMediaSourceID := query.Get("MediaSourceId")
	if virtualMediaSourceID != "" {
		if msResolved := a.IDStore.ResolveVirtualID(virtualMediaSourceID); msResolved != nil {
			query.Set("MediaSourceId", msResolved.OriginalID)
			if msResolved.ServerIndex != resolved.ServerIndex {
				if client := a.Upstream.GetClient(msResolved.ServerIndex); client != nil && client.IsOnline() {
					if a.Logger != nil {
						a.Logger.Infof("Stream: switching to server [%s] for MediaSourceId %s", client.Name, virtualMediaSourceID)
					}
					actualClient = client
					// Update actualOriginalID to match this server's copy of the item
					found := false
					for _, other := range resolved.OtherInstances {
						if other.ServerIndex == msResolved.ServerIndex {
							actualOriginalID = other.OriginalID
							found = true
							break
						}
					}
					if !found && msResolved.ServerIndex == resolved.ServerIndex {
						actualOriginalID = resolved.OriginalID
						found = true
					}
				}
			}
		} else {
			if a.Logger != nil {
				a.Logger.Warnf("Stream: MediaSourceId %s cannot be resolved to any server", virtualMediaSourceID)
			}
		}
	}

	if playSessionID := query.Get("PlaySessionId"); playSessionID != "" {
		if psResolved := a.IDStore.ResolveVirtualID(playSessionID); psResolved != nil {
			query.Set("PlaySessionId", psResolved.OriginalID)
		}
	}
	// Replace proxy token with upstream's access token for stream auth
	query.Del("api_key")
	query.Del("ApiKey")
	token := actualClient.getAccessToken()
	if token != "" {
		query.Set("api_key", token)
	}
	upstreamPath := "/Videos/" + actualOriginalID + "/" + rest
	if a.Logger != nil {
		a.Logger.Infof("Stream: /Videos/%s/%s → [%s] %s (using token: %v)",
			virtualItemID, rest, actualClient.Name, upstreamPath, token != "")
	}
	// Redirect mode: return 302 to upstream stream URL
	playbackMode := actualClient.Config.PlaybackMode
	if playbackMode == "" {
		playbackMode = a.ConfigStore.Snapshot().Playback.Mode
	}
	if playbackMode == "redirect" {
		redirectURL := actualClient.BuildURL(upstreamPath, query, true)
		if a.Logger != nil {
			a.Logger.Debugf("Stream redirect: /Videos/%s/%s → 302 %s", virtualItemID, rest, redirectURL)
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	// Forward Range and Accept headers from client for seeking / partial content
	streamHeaders := http.Header{}
	for _, h := range []string{"Range", "Accept", "Accept-Encoding", "Accept-Language"} {
		if v := r.Header.Get(h); v != "" {
			streamHeaders.Set(h, v)
		}
	}
	if a.Logger != nil {
		a.Logger.Debugf("Stream request headers: Range=%q, Accept=%q, AE=%q",
			r.Header.Get("Range"), r.Header.Get("Accept"), r.Header.Get("Accept-Encoding"))
	}
	resp, err := actualClient.Stream(r.Context(), requestContextFrom(r.Context()), a.Identity, upstreamPath, query, streamHeaders)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client disconnected or timed out — not a server error
		}
		if a.Logger != nil {
			a.Logger.Errorf("Stream error: /Videos/%s/%s: %s", virtualItemID, rest, err.Error())
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	isM3U8 := strings.Contains(contentType, "mpegurl") || strings.HasSuffix(strings.ToLower(rest), ".m3u8")
	if isM3U8 {
		body, _ := io.ReadAll(resp.Body)
		proxyToken := ""
		if reqCtx := requestContextFrom(r.Context()); reqCtx != nil {
			proxyToken = reqCtx.ProxyToken
		}
		manifest := RewriteM3U8ForItem(string(body), actualClient.BuildURL(upstreamPath, query, true), virtualItemID, proxyToken)
		w.Header().Set("Content-Type", "application/x-mpegURL")
		_, _ = io.WriteString(w, manifest)
		return
	}

	if a.Logger != nil {
		a.Logger.Infof("Stream upstream response: Status=%d, Type=%q, Len=%s, Encoding=%q, Range=%q",
			resp.StatusCode, contentType, resp.Header.Get("Content-Length"),
			resp.Header.Get("Content-Encoding"), resp.Header.Get("Content-Range"))
	}

	for _, header := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Cache-Control", "ETag", "Last-Modified", "Transfer-Encoding",
		"Content-Disposition", "Content-Encoding", "Date", "Server",
	} {
		if value := resp.Header.Get(header); value != "" {
			if strings.EqualFold(header, "Transfer-Encoding") && strings.Contains(strings.ToLower(value), "chunked") {
				continue
			}
			w.Header().Set(header, value)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (a *App) handleAudioProxy(w http.ResponseWriter, r *http.Request) {
	virtualItemID := r.PathValue("itemId")
	query := cloneValues(r.URL.Query())
	if a.Logger != nil {
		a.Logger.Debugf("Audio stream request: itemId=%s, query=%s", virtualItemID, query.Encode())
	}
	resolved := a.resolveRouteID(virtualItemID)
	if resolved == nil {
		if a.Logger != nil {
			a.Logger.Warnf("Audio stream: itemId=%s not found in mappings", virtualItemID)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	rest := r.PathValue("rest")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Stream not found"})
		return
	}
	// Resolve virtual MediaSourceId in subtitle/attachment paths
	rest = resolveMediaSourceInPath(rest, a.IDStore)
	actualClient := resolved.Client
	actualOriginalID := resolved.OriginalID

	virtualMediaSourceID := query.Get("MediaSourceId")
	if virtualMediaSourceID != "" {
		if msResolved := a.IDStore.ResolveVirtualID(virtualMediaSourceID); msResolved != nil {
			query.Set("MediaSourceId", msResolved.OriginalID)
			if msResolved.ServerIndex != resolved.ServerIndex {
				if client := a.Upstream.GetClient(msResolved.ServerIndex); client != nil && client.IsOnline() {
					if a.Logger != nil {
						a.Logger.Infof("Audio stream: switching to server [%s] for MediaSourceId %s", client.Name, virtualMediaSourceID)
					}
					actualClient = client
					found := false
					for _, other := range resolved.OtherInstances {
						if other.ServerIndex == msResolved.ServerIndex {
							actualOriginalID = other.OriginalID
							found = true
							break
						}
					}
					if !found && msResolved.ServerIndex == resolved.ServerIndex {
						actualOriginalID = resolved.OriginalID
						found = true
					}
				}
			}
		}
	}

	if playSessionID := query.Get("PlaySessionId"); playSessionID != "" {
		if psResolved := a.IDStore.ResolveVirtualID(playSessionID); psResolved != nil {
			query.Set("PlaySessionId", psResolved.OriginalID)
		}
	}
	// Replace proxy token with upstream's access token for stream auth
	query.Del("api_key")
	query.Del("ApiKey")
	if token := actualClient.getAccessToken(); token != "" {
		query.Set("api_key", token)
	}
	upstreamPath := "/Audio/" + actualOriginalID + "/" + rest
	if a.Logger != nil {
		a.Logger.Infof("Stream: /Audio/%s/%s → [%s] %s", virtualItemID, rest, actualClient.Name, upstreamPath)
	}
	// Redirect mode: return 302 to upstream stream URL
	playbackMode := actualClient.Config.PlaybackMode
	if playbackMode == "" {
		playbackMode = a.ConfigStore.Snapshot().Playback.Mode
	}
	if playbackMode == "redirect" {
		redirectURL := actualClient.BuildURL(upstreamPath, query, true)
		if a.Logger != nil {
			a.Logger.Debugf("Stream redirect: /Audio/%s/%s → 302 %s", virtualItemID, rest, redirectURL)
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	// Forward Range and Accept headers from client for seeking / partial content
	streamHeaders := http.Header{}
	for _, h := range []string{"Range", "Accept", "Accept-Encoding", "Accept-Language"} {
		if v := r.Header.Get(h); v != "" {
			streamHeaders.Set(h, v)
		}
	}
	resp, err := actualClient.Stream(r.Context(), requestContextFrom(r.Context()), a.Identity, upstreamPath, query, streamHeaders)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client disconnected or timed out — not a server error
		}
		if a.Logger != nil {
			a.Logger.Errorf("Stream error: /Audio/%s/%s: %s", virtualItemID, rest, err.Error())
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	isM3U8 := strings.Contains(contentType, "mpegurl") || strings.HasSuffix(strings.ToLower(rest), ".m3u8")
	if isM3U8 {
		body, _ := io.ReadAll(resp.Body)
		proxyToken := ""
		if reqCtx := requestContextFrom(r.Context()); reqCtx != nil {
			proxyToken = reqCtx.ProxyToken
		}
		manifest := RewriteM3U8ForItem(string(body), actualClient.BuildURL(upstreamPath, query, true), virtualItemID, proxyToken)
		w.Header().Set("Content-Type", "application/x-mpegURL")
		_, _ = io.WriteString(w, manifest)
		return
	}

	if a.Logger != nil {
		a.Logger.Infof("Audio stream upstream response: Status=%d, Type=%q, Len=%s",
			resp.StatusCode, contentType, resp.Header.Get("Content-Length"))
	}

	for _, header := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Cache-Control", "ETag", "Last-Modified", "Transfer-Encoding",
		"Content-Disposition", "Content-Encoding", "Date", "Server",
	} {
		if value := resp.Header.Get(header); value != "" {
			if strings.EqualFold(header, "Transfer-Encoding") && strings.Contains(strings.ToLower(value), "chunked") {
				continue
			}
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (a *App) handleDeleteActiveEncodings(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	serverIndex := -1
	if playSessionID := query.Get("PlaySessionId"); playSessionID != "" {
		if resolved := a.IDStore.ResolveVirtualID(playSessionID); resolved != nil {
			query.Set("PlaySessionId", resolved.OriginalID)
			serverIndex = resolved.ServerIndex
		}
	}
	if serverIndex >= 0 {
		if client := a.Upstream.GetClient(serverIndex); client != nil && client.IsOnline() {
			_ = a.forwardNoContent(r, client, http.MethodDelete, "/Videos/ActiveEncodings", query, nil)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, client := range a.Upstream.OnlineClients() {
		_ = a.forwardNoContent(r, client, http.MethodDelete, "/Videos/ActiveEncodings", query, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}
