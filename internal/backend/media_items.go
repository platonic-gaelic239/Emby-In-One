package backend

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (a *App) handleItemsCollection(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	if !hasBatchIDQuery(query) {
		a.handleFallbackProxy(w, r)
		return
	}
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	if len(clients) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}

	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}

	tasks := make([]upstreamTask, len(clients))
	for i, client := range clients {
		c := client
		tasks[i] = upstreamTask{
			index: i,
			fn: func(bgCtx context.Context) upstreamItemsResult {
				serverQuery, ok := translateBatchIDQueryForServer(query, c.ServerIndex, a.IDStore)
				if !ok {
					return upstreamItemsResult{}
				}
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, "/Items", serverQuery, nil)
				if err != nil {
					return upstreamItemsResult{}
				}
				return upstreamItemsResult{ServerIndex: c.ServerIndex, Items: asItems(payload)}
			},
		}
	}

	collected := a.aggregateUpstreams(r.Context(), aggregationConfig{
		gracePeriod:   time.Duration(cfg.Timeouts.SearchGracePeriod) * time.Millisecond,
		globalTimeout: globalTimeout,
	}, tasks)
	merged := a.mergedItemsPayload(collected)
	a.overlayLocalUserDataItems(r, asItems(merged))
	writeJSON(w, http.StatusOK, merged)
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
		a.overlayLocalUserDataItems(r, asItems(payload))
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
		a.overlayLocalUserDataItems(r, asMaps)
		writeJSON(w, http.StatusOK, paginateItems(asMaps, r.URL.Query()))
		return
	}
	writeJSON(w, http.StatusOK, merged)
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
		a.overlayLocalUserDataItems(r, items)
		writeJSON(w, http.StatusOK, items)
		return
	}
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}

	tasks := make([]upstreamTask, len(clients))
	for i, client := range clients {
		c := client
		tasks[i] = upstreamTask{
			index: i,
			fn: func(bgCtx context.Context) upstreamItemsResult {
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, "/Users/"+c.UserID+"/Items/Latest", query, nil)
				if err != nil {
					return upstreamItemsResult{}
				}
				items := asItems(payload)
				a.rewriteItems(items, c.ServerIndex)
				return upstreamItemsResult{ServerIndex: c.ServerIndex, Items: items}
			},
		}
	}

	collected := a.aggregateUpstreams(r.Context(), aggregationConfig{
		gracePeriod:   time.Duration(cfg.Timeouts.LatestGracePeriod) * time.Millisecond,
		globalTimeout: globalTimeout,
	}, tasks)
	allItems := make([]map[string]any, 0)
	for _, result := range collected {
		allItems = append(allItems, result.Items...)
	}
	a.overlayLocalUserDataItems(r, allItems)
	writeJSON(w, http.StatusOK, allItems)
}

func (a *App) handleUserItemByID(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	reqCtx := requestContextFrom(r.Context())
	if !a.isServerAllowed(reqCtx, resolved.ServerIndex) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
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
		a.overlayLocalUserData(r, r.PathValue("itemId"), payload)
		writeJSON(w, http.StatusOK, payload)
		return
	}

	var base map[string]any
	var baseServerIndex int
	allMediaSources := []map[string]any{}

	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}
	bgCtx, bgCancel := context.WithTimeout(context.Background(), globalTimeout)

	type instanceResult struct {
		serverIndex  int
		data         map[string]any
		mediaSources []map[string]any
	}
	resultCh := make(chan *instanceResult, len(instances))

	for _, inst := range instances {
		go func(si seriesInstance) {
			payload, err := si.Client.RequestJSON(bgCtx, requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+si.Client.UserID+"/Items/"+si.OriginalID, cloneValues(r.URL.Query()), nil)
			if err != nil {
				resultCh <- nil
				return
			}
			data, ok := payload.(map[string]any)
			if !ok {
				resultCh <- nil
				return
			}
			var mediaSources []map[string]any
			for _, raw := range asItems(map[string]any{"Items": data["MediaSources"]}) {
				ms := deepCloneMap(raw)
				if originalID, _ := ms["Id"].(string); originalID != "" {
					ms["Id"] = a.IDStore.GetOrCreateVirtualID(originalID, si.ServerIndex)
				}
				if client := a.Upstream.GetClient(si.ServerIndex); client != nil {
					name, _ := ms["Name"].(string)
					if name == "" {
						name = "Version"
					}
					ms["Name"] = name + " [" + client.Name + "]"
				}
				mediaSources = append(mediaSources, ms)
			}
			resultCh <- &instanceResult{serverIndex: si.ServerIndex, data: data, mediaSources: mediaSources}
		}(inst)
	}

	gracePeriod := time.Duration(cfg.Timeouts.MetadataGracePeriod) * time.Millisecond
	var graceTimer <-chan time.Time
	received := 0

	for received < len(instances) {
		select {
		case res := <-resultCh:
			received++
			if res != nil {
				if base == nil {
					base = deepCloneMap(res.data)
					baseServerIndex = res.serverIndex
				}
				allMediaSources = append(allMediaSources, res.mediaSources...)
				if graceTimer == nil && gracePeriod > 0 {
					graceTimer = time.After(gracePeriod)
				}
			}
		case <-graceTimer:
			goto metadataDone
		case <-r.Context().Done():
			goto metadataDone
		}
	}
metadataDone:
	bgCancel()
	if base == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream request failed"})
		return
	}
	mediaSources := make([]any, 0, len(allMediaSources))
	for _, mediaSource := range allMediaSources {
		mediaSources = append(mediaSources, mediaSource)
	}
	// delete-and-restore: prevent rewriteResponseIDs from double-wrapping
	// already-virtualised MediaSource IDs and creating orphan mappings
	delete(base, "MediaSources")
	rewriteResponseIDs(base, baseServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	base["MediaSources"] = mediaSources
	a.overlayLocalUserData(r, r.PathValue("itemId"), base)
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
	a.overlayLocalUserData(r, r.PathValue("itemId"), payload)
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
	a.overlayLocalUserDataItems(r, asItems(payload))
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
	clients := a.allowedClients(reqCtx)
	if len(clients) == 0 {
		return nil
	}
	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}

	tasks := make([]upstreamTask, len(clients))
	for i, client := range clients {
		c := client
		tasks[i] = upstreamTask{
			index: i,
			fn: func(bgCtx context.Context) upstreamItemsResult {
				serverQuery := cloneValues(query)
				if hasBatchIDQuery(serverQuery) {
					translated, ok := translateBatchIDQueryForServer(serverQuery, c.ServerIndex, a.IDStore)
					if !ok {
						return upstreamItemsResult{}
					}
					serverQuery = translated
				}
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, strings.Replace(pathTemplate, "%s", c.UserID, 1), serverQuery, body)
				if err != nil {
					return upstreamItemsResult{}
				}
				return upstreamItemsResult{ServerIndex: c.ServerIndex, Items: asItems(payload)}
			},
		}
	}

	return a.aggregateUpstreams(ctx, aggregationConfig{
		gracePeriod:   time.Duration(cfg.Timeouts.SearchGracePeriod) * time.Millisecond,
		globalTimeout: globalTimeout,
	}, tasks)
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
