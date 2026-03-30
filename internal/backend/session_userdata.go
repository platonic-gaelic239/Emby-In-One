package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

func (a *App) registerSessionAndUserStateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /Sessions/Playing", a.withContext(a.requireAuth(a.handleSessionPlaying)))
	mux.HandleFunc("POST /Sessions/Playing/Progress", a.withContext(a.requireAuth(a.handleSessionPlayingProgress)))
	mux.HandleFunc("POST /Sessions/Playing/Stopped", a.withContext(a.requireAuth(a.handleSessionPlayingStopped)))
	mux.HandleFunc("POST /Sessions/Capabilities", a.withContext(a.requireAuth(a.handleSessionsCapabilities)))
	mux.HandleFunc("POST /Sessions/Capabilities/Full", a.withContext(a.requireAuth(a.handleSessionsCapabilitiesFull)))
	mux.HandleFunc("POST /Users/{userId}/PlayingItems/{itemId}", a.withContext(a.requireAuth(a.handleUserPlayingItemStart)))
	mux.HandleFunc("DELETE /Users/{userId}/PlayingItems/{itemId}", a.withContext(a.requireAuth(a.handleUserPlayingItemStop)))
	mux.HandleFunc("POST /Users/{userId}/Items/{itemId}/UserData", a.withContext(a.requireAuth(a.handleUserItemUserData)))
	mux.HandleFunc("POST /Users/{userId}/FavoriteItems/{itemId}", a.withContext(a.requireAuth(a.handleFavoriteItemAdd)))
	mux.HandleFunc("DELETE /Users/{userId}/FavoriteItems/{itemId}", a.withContext(a.requireAuth(a.handleFavoriteItemRemove)))
}

func decodeOptionalJSON(r *http.Request) (any, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (a *App) translateSessionBodyIDs(body map[string]any) (int, bool) {
	serverIndex := -1
	// 1. Identify target server: prefer MediaSourceId, then ItemId, then PlaySessionId
	for _, key := range []string{"MediaSourceId", "ItemId", "PlaySessionId"} {
		text, _ := body[key].(string)
		if text == "" {
			continue
		}
		resolved := a.IDStore.ResolveVirtualID(text)
		if resolved == nil {
			// Fallback: client sent original upstream ID instead of virtual ID
			resolved = a.IDStore.ResolveByOriginalID(text)
		}
		if resolved != nil {
			serverIndex = resolved.ServerIndex
			break
		}
	}

	if serverIndex < 0 {
		// Last resort: check which server last served a PlaybackInfo for ItemId
		if itemID, _ := body["ItemId"].(string); itemID != "" {
			if idx, ok := a.IDStore.GetActiveStream(itemID); ok {
				serverIndex = idx
			}
		}
	}

	if serverIndex < 0 {
		return -1, false
	}

	// 2. Translate all IDs in body to the target server's original IDs
	for _, key := range []string{"MediaSourceId", "ItemId", "PlaySessionId"} {
		text, _ := body[key].(string)
		if text == "" {
			continue
		}
		resolved := a.IDStore.ResolveVirtualID(text)
		if resolved == nil {
			// Client sent original upstream ID — no translation needed if it's already correct
			resolved = a.IDStore.ResolveByOriginalID(text)
			if resolved == nil {
				continue
			}
			// Already an original ID for the right server, nothing to rewrite
			if resolved.ServerIndex == serverIndex {
				continue
			}
		}

		if resolved.ServerIndex == serverIndex {
			body[key] = resolved.OriginalID
		} else {
			// Cross-server instance: try to find an instance of this item on the target server
			found := false
			for _, inst := range resolved.OtherInstances {
				if inst.ServerIndex == serverIndex {
					body[key] = inst.OriginalID
					found = true
					break
				}
			}
			if !found && a.Logger != nil {
				a.Logger.Warnf("Session translation: %s (%s) has no instance on target server %d",
					key, text, serverIndex)
				// Keep virtual or remove? Emby might fail if it's virtual.
				// Better to keep it virtual if we can't find an original, or maybe the upstream will just ignore it.
			}
		}
	}

	if a.Logger != nil {
		a.Logger.Debugf("Session translation: TargetServer=%d, MediaSourceId=%v, ItemId=%v, PlaySessionId=%v",
			serverIndex, body["MediaSourceId"], body["ItemId"], body["PlaySessionId"])
	}
	return serverIndex, serverIndex >= 0
}

func (a *App) translateMediaSourceQuery(values url.Values) {
	if mediaSourceID := values.Get("MediaSourceId"); mediaSourceID != "" {
		if resolved := a.IDStore.ResolveVirtualID(mediaSourceID); resolved != nil {
			values.Set("MediaSourceId", resolved.OriginalID)
		}
	}
}

func (a *App) performUpstream(ctx *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) (*http.Response, error) {
	return client.doRequest(ctx.Context(), method, path, query, body, client.requestHeaders(requestContextFrom(ctx.Context()), a.Identity), false)
}

func readUpstreamJSONOrNoContent(resp *http.Response) (int, any, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, nil, fmt.Errorf("upstream request failed: %s %s", resp.Status, string(bytes.TrimSpace(payload)))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}

func (a *App) forwardNoContent(r *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) error {
	resp, err := a.performUpstream(r, client, method, path, query, body)
	if err != nil {
		return err
	}
	_, _, err = readUpstreamJSONOrNoContent(resp)
	return err
}

func (a *App) forwardJSONOrNoContent(r *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) (int, any, error) {
	resp, err := a.performUpstream(r, client, method, path, query, body)
	if err != nil {
		return 0, nil, err
	}
	return readUpstreamJSONOrNoContent(resp)
}

func asBodyMap(payload any) (map[string]any, bool) {
	if payload == nil {
		return map[string]any{}, true
	}
	body, ok := payload.(map[string]any)
	return body, ok
}

func (a *App) handleSessionPlaying(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	body, ok := asBodyMap(payload)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid session payload"})
		return
	}
	serverIndex, found := a.translateSessionBodyIDs(body)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Cannot determine target server"})
		return
	}
	client := a.Upstream.GetClient(serverIndex)
	if client == nil || !client.IsOnline() {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Server not found"})
		return
	}
	if err := a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Playing", nil, body); err != nil {
		if a.Logger != nil {
			a.Logger.Warnf("Sessions/Playing upstream error (server %d): %v", serverIndex, err)
		}
		// Return 204 anyway — session reporting is best-effort and client must not break
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionPlayingProgress(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeOptionalJSON(r)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, ok := asBodyMap(payload)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	serverIndex, found := a.translateSessionBodyIDs(body)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	client := a.Upstream.GetClient(serverIndex)
	if client == nil || !client.IsOnline() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Playing/Progress", nil, body)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionPlayingStopped(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeOptionalJSON(r)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, ok := asBodyMap(payload)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	serverIndex, found := a.translateSessionBodyIDs(body)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	client := a.Upstream.GetClient(serverIndex)
	if client == nil || !client.IsOnline() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Playing/Stopped", nil, body)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionsCapabilities(w http.ResponseWriter, r *http.Request) {
	body, err := decodeOptionalJSON(r)
	if err == nil {
		for _, client := range a.Upstream.OnlineClients() {
			_ = a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Capabilities", cloneValues(r.URL.Query()), body)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionsCapabilitiesFull(w http.ResponseWriter, r *http.Request) {
	body, err := decodeOptionalJSON(r)
	if err == nil {
		for _, client := range a.Upstream.OnlineClients() {
			_ = a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Capabilities/Full", nil, body)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleUserPlayingItemStart(w http.ResponseWriter, r *http.Request) {
	a.handleUserPlayingItem(w, r, http.MethodPost)
}

func (a *App) handleUserPlayingItemStop(w http.ResponseWriter, r *http.Request) {
	a.handleUserPlayingItem(w, r, http.MethodDelete)
}

func (a *App) handleUserPlayingItem(w http.ResponseWriter, r *http.Request, method string) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	query := cloneValues(r.URL.Query())
	a.translateMediaSourceQuery(query)
	path := "/Users/" + resolved.Client.UserID + "/PlayingItems/" + resolved.OriginalID
	_ = a.forwardNoContent(r, resolved.Client, method, path, query, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleUserItemUserData(w http.ResponseWriter, r *http.Request) {
	a.handleResolvedJSONMutation(w, r, http.MethodPost, "/Users/%s/Items/%s/UserData")
}

func (a *App) handleFavoriteItemAdd(w http.ResponseWriter, r *http.Request) {
	a.handleResolvedJSONMutation(w, r, http.MethodPost, "/Users/%s/FavoriteItems/%s")
}

func (a *App) handleFavoriteItemRemove(w http.ResponseWriter, r *http.Request) {
	a.handleResolvedJSONMutation(w, r, http.MethodDelete, "/Users/%s/FavoriteItems/%s")
}

func (a *App) handleResolvedJSONMutation(w http.ResponseWriter, r *http.Request, method, pathFormat string) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	body, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	status, payload, err := a.forwardJSONOrNoContent(r, resolved.Client, method, fmt.Sprintf(pathFormat, resolved.Client.UserID, resolved.OriginalID), nil, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
		return
	}
	if payload == nil {
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
		return
	}
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerIndex, a.IDStore, cfg.Server.ID, a.Auth.ProxyUserID())
	writeJSON(w, status, payload)
}
