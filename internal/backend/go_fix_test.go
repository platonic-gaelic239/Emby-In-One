package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdminUpstreamCreateRejectsOfflineDraft(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer broken.Close()

	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")

		rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream", map[string]any{
			"name":     "Broken",
			"url":      broken.URL,
			"username": "u1",
			"password": "p1",
		}, token)
		if rr.Code == http.StatusOK {
			t.Fatalf("create upstream unexpectedly succeeded: %s", rr.Body.String())
		}
		if got := len(app.ConfigStore.Snapshot().Upstream); got != 0 {
			t.Fatalf("upstream config count = %d, want 0", got)
		}
		if got := len(app.Upstream.Clients()); got != 0 {
			t.Fatalf("runtime upstream count = %d, want 0", got)
		}
	})
}

func TestAdminUpstreamUpdateRejectsOfflineDraftAndKeepsExistingClient(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info":
			_ = json.NewEncoder(w).Encode(map[string]any{"Version": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Views":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": "view-a", "Name": "Library A"}}, "TotalRecordCount": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer good.Close()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer broken.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n", good.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		before := app.ConfigStore.Snapshot().Upstream[0]

		rr := doJSONRequest(t, handler, http.MethodPut, "/admin/api/upstream/0", map[string]any{
			"name":     "Broken",
			"url":      broken.URL,
			"username": "u2",
			"password": "p2",
		}, token)
		if rr.Code == http.StatusOK {
			t.Fatalf("update upstream unexpectedly succeeded: %s", rr.Body.String())
		}

		after := app.ConfigStore.Snapshot().Upstream[0]
		if after.URL != before.URL || after.Username != before.Username || after.Name != before.Name {
			t.Fatalf("upstream config mutated after failed update: before=%#v after=%#v", before, after)
		}
		clients := app.Upstream.Clients()
		if len(clients) != 1 || !clients[0].Online || clients[0].BaseURL != strings.TrimRight(good.URL, "/") {
			t.Fatalf("runtime client mutated after failed update: %#v", clients)
		}
	})
}

func TestAdminSettingsUpdateKeepsUpstreamOnlineAfterCommit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n", upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")

		statusBefore := doJSONRequest(t, handler, http.MethodGet, "/admin/api/status", nil, token)
		if statusBefore.Code != http.StatusOK {
			t.Fatalf("status before = %d body=%s", statusBefore.Code, statusBefore.Body.String())
		}
		var before map[string]any
		if err := json.Unmarshal(statusBefore.Body.Bytes(), &before); err != nil {
			t.Fatalf("unmarshal status before: %v", err)
		}
		if before["upstreamOnline"].(float64) != 1 {
			t.Fatalf("upstream online before = %#v", before)
		}

		settingsRR := doJSONRequest(t, handler, http.MethodPut, "/admin/api/settings", map[string]any{
			"serverName": "Renamed Server",
		}, token)
		if settingsRR.Code != http.StatusOK {
			t.Fatalf("settings update status = %d body=%s", settingsRR.Code, settingsRR.Body.String())
		}

		statusAfter := doJSONRequest(t, handler, http.MethodGet, "/admin/api/status", nil, token)
		if statusAfter.Code != http.StatusOK {
			t.Fatalf("status after = %d body=%s", statusAfter.Code, statusAfter.Body.String())
		}
		var after map[string]any
		if err := json.Unmarshal(statusAfter.Body.Bytes(), &after); err != nil {
			t.Fatalf("unmarshal status after: %v", err)
		}
		if after["upstreamOnline"].(float64) != 1 {
			t.Fatalf("upstream online after settings save = %#v", after)
		}
	})
}

func TestAdminUpstreamReorderPreservesExistingVirtualIDRouting(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/item-a":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "item-a", "Name": "From A"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-b", "User": map[string]any{"Id": "user-b"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/item-b":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "item-b", "Name": "From B"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer secondary.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n  - name: \"B\"\n    url: %q\n    username: \"u2\"\n    password: \"p2\"\n", primary.URL, secondary.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		virtualItemA := app.IDStore.GetOrCreateVirtualID("item-a", 0)
		virtualItemB := app.IDStore.GetOrCreateVirtualID("item-b", 1)

		reorderRR := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream/reorder", map[string]any{
			"fromIndex": 0,
			"toIndex":   1,
		}, token)
		if reorderRR.Code != http.StatusOK {
			t.Fatalf("reorder status = %d body=%s", reorderRR.Code, reorderRR.Body.String())
		}

		itemARR := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtualItemA, nil, token)
		if itemARR.Code != http.StatusOK {
			t.Fatalf("item A after reorder status = %d body=%s", itemARR.Code, itemARR.Body.String())
		}
		var itemA map[string]any
		if err := json.Unmarshal(itemARR.Body.Bytes(), &itemA); err != nil {
			t.Fatalf("unmarshal item A: %v", err)
		}
		if itemA["Name"] != "From A" {
			t.Fatalf("item A routed to wrong upstream: %#v", itemA)
		}

		itemBRR := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtualItemB, nil, token)
		if itemBRR.Code != http.StatusOK {
			t.Fatalf("item B after reorder status = %d body=%s", itemBRR.Code, itemBRR.Body.String())
		}
		var itemB map[string]any
		if err := json.Unmarshal(itemBRR.Body.Bytes(), &itemB); err != nil {
			t.Fatalf("unmarshal item B: %v", err)
		}
		if itemB["Name"] != "From B" {
			t.Fatalf("item B routed to wrong upstream: %#v", itemB)
		}
	})
}

func TestUserViewsMergesAcrossUpstreams(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Views":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": "view-a", "Name": "Movies"}}, "TotalRecordCount": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-b", "User": map[string]any{"Id": "user-b"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-b/Views":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": "view-b", "Name": "Movies"}}, "TotalRecordCount": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer secondary.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n  - name: \"B\"\n    url: %q\n    username: \"u2\"\n    password: \"p2\"\n", primary.URL, secondary.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/Views", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("views status = %d body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal views: %v", err)
		}
		items, _ := payload["Items"].([]any)
		if len(items) != 2 {
			t.Fatalf("views count = %d want 2 payload=%#v", len(items), payload)
		}
		joinedNames := []string{}
		for _, raw := range items {
			item := raw.(map[string]any)
			joinedNames = append(joinedNames, item["Name"].(string))
			if item["Id"] == "view-a" || item["Id"] == "view-b" {
				t.Fatalf("expected rewritten view id, got %#v", item)
			}
		}
		joined := strings.Join(joinedNames, " | ")
		if !strings.Contains(joined, "(A)") || !strings.Contains(joined, "(B)") {
			t.Fatalf("expected tagged view names, got %q", joined)
		}
	})
}

func TestFallbackProxyRejectsAmbiguousMultiServerRequests(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Unknown":
			_ = json.NewEncoder(w).Encode(map[string]any{"server": "A"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-b", "User": map[string]any{"Id": "user-b"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Unknown":
			_ = json.NewEncoder(w).Encode(map[string]any{"server": "B"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer secondary.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n  - name: \"B\"\n    url: %q\n    username: \"u2\"\n    password: \"p2\"\n", primary.URL, secondary.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodGet, "/Unknown", nil, token)
		if rr.Code == http.StatusOK {
			t.Fatalf("ambiguous fallback unexpectedly succeeded: %s", rr.Body.String())
		}
	})
}

func TestFallbackProxyPreservesPlainTextRequestBody(t *testing.T) {
	var seenBody string
	var seenType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/TextEcho":
			raw, _ := io.ReadAll(r.Body)
			seenBody = string(raw)
			seenType = r.Header.Get("Content-Type")
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n", upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		req := httptest.NewRequest(http.MethodPost, "/Users/"+app.Auth.ProxyUserID()+"/TextEcho", strings.NewReader("hello world"))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("X-Emby-Token", token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("plain text fallback status = %d body=%s", rr.Code, rr.Body.String())
		}
		if seenBody != "hello world" {
			t.Fatalf("upstream saw body %q, want raw plain text", seenBody)
		}
		if seenType != "text/plain" {
			t.Fatalf("upstream content-type = %q, want text/plain", seenType)
		}
	})
}

func TestAdminUpstreamCreateAllowsPassthroughWithoutCapturedHeaders(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing live client identity", http.StatusUnauthorized)
	}))
	defer broken.Close()

	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream", map[string]any{
			"name":        "Passthrough",
			"url":         broken.URL,
			"username":    "u1",
			"password":    "p1",
			"spoofClient": "passthrough",
		}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("create passthrough status = %d, want 200 body=%s", rr.Code, rr.Body.String())
		}
		if got := len(app.ConfigStore.Snapshot().Upstream); got != 1 {
			t.Fatalf("upstream config count = %d, want 1", got)
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal create passthrough response: %v", err)
		}
		if online, _ := payload["online"].(bool); online {
			t.Fatalf("passthrough create unexpectedly reported online: %#v", payload)
		}
		if _, ok := payload["warning"]; !ok {
			t.Fatalf("passthrough create response missing warning: %#v", payload)
		}
	})
}

func TestAdminUpstreamCreatePassthroughIgnoresBrowserOnlyHeaders(t *testing.T) {
	var authHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte{0x9d, 0x00, 0x00})
	}))
	defer upstream.Close()

	withTempApp(t, func(app *App, handler http.Handler) {
		loginReq := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewBufferString(`{"Username":"admin","Pw":"secret"}`))
		loginReq.Header.Set("Content-Type", "application/json")
		loginReq.Header.Set("User-Agent", "Mozilla/5.0")
		loginReq.Header.Set("Accept-Encoding", "br")
		loginRR := httptest.NewRecorder()
		handler.ServeHTTP(loginRR, loginReq)
		if loginRR.Code != http.StatusOK {
			t.Fatalf("browser-style login status = %d body=%s", loginRR.Code, loginRR.Body.String())
		}
		var loginPayload map[string]any
		if err := json.Unmarshal(loginRR.Body.Bytes(), &loginPayload); err != nil {
			t.Fatalf("unmarshal browser-style login response: %v", err)
		}
		token, _ := loginPayload["AccessToken"].(string)
		if token == "" {
			t.Fatalf("browser-style login missing token: %#v", loginPayload)
		}

		payload, err := json.Marshal(map[string]any{
			"name":        "Browser Passthrough",
			"url":         upstream.URL,
			"username":    "u1",
			"password":    "p1",
			"spoofClient": "passthrough",
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Token", token)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		req.Header.Set("Accept-Encoding", "br")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("create passthrough with browser headers status = %d, want 200 body=%s", rr.Code, rr.Body.String())
		}
		if got := len(app.ConfigStore.Snapshot().Upstream); got != 1 {
			t.Fatalf("upstream config count = %d, want 1", got)
		}
		var response map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal create response: %v", err)
		}
		if online, _ := response["online"].(bool); online {
			t.Fatalf("browser-only passthrough unexpectedly reported online: %#v", response)
		}
		if _, ok := response["warning"]; !ok {
			t.Fatalf("browser-only passthrough response missing warning: %#v", response)
		}
		if authHits.Load() == 0 {
			t.Fatalf("expected post-commit passthrough reconnect attempt to hit upstream at least once")
		}
	})
}

func TestAdminUpstreamUpdateAllowsPassthroughWithoutCapturedHeaders(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer good.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing live client identity", http.StatusUnauthorized)
	}))
	defer broken.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n", good.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodPut, "/admin/api/upstream/0", map[string]any{
			"name":        "Recovered Later",
			"url":         broken.URL,
			"username":    "u2",
			"password":    "p2",
			"spoofClient": "passthrough",
		}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("update passthrough status = %d, want 200 body=%s", rr.Code, rr.Body.String())
		}
		updated := app.ConfigStore.Snapshot().Upstream[0]
		if updated.URL != broken.URL || updated.SpoofClient != "passthrough" {
			t.Fatalf("passthrough update was not saved: %#v", updated)
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal update passthrough response: %v", err)
		}
		if online, _ := payload["online"].(bool); online {
			t.Fatalf("passthrough update unexpectedly reported online: %#v", payload)
		}
		if _, ok := payload["warning"]; !ok {
			t.Fatalf("passthrough update response missing warning: %#v", payload)
		}
	})
}
