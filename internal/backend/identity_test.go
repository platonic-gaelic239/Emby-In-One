package backend

import (
	"net/http"
	"testing"
)

func TestClientIdentityServiceIsTokenScoped(t *testing.T) {
	svc := NewClientIdentityService()
	svc.SetCaptured("token-a", http.Header{
		"User-Agent":            []string{"UA-A"},
		"X-Emby-Client":         []string{"Client-A"},
		"X-Emby-Device-Name":    []string{"Device-A"},
		"X-Emby-Device-Id":      []string{"device-a"},
		"X-Emby-Client-Version": []string{"1.0"},
	})
	svc.SetCaptured("token-b", http.Header{
		"User-Agent":            []string{"UA-B"},
		"X-Emby-Client":         []string{"Client-B"},
		"X-Emby-Device-Name":    []string{"Device-B"},
		"X-Emby-Device-Id":      []string{"device-b"},
		"X-Emby-Client-Version": []string{"2.0"},
	})

	if got := svc.GetCaptured("token-a").Get("User-Agent"); got != "UA-A" {
		t.Fatalf("token-a user agent = %q, want UA-A", got)
	}
	if got := svc.GetCaptured("token-b").Get("User-Agent"); got != "UA-B" {
		t.Fatalf("token-b user agent = %q, want UA-B", got)
	}
	info := svc.GetInfo()
	if info == nil || info.UserAgent != "UA-B" {
		t.Fatalf("latest captured info = %#v, want UA-B", info)
	}
}

func TestResolvePassthroughHeadersPreferenceOrder(t *testing.T) {
	svc := NewClientIdentityService()
	svc.SetCaptured("token-a", http.Header{
		"User-Agent":            []string{"UA-A"},
		"X-Emby-Client":         []string{"Client-A"},
		"X-Emby-Device-Name":    []string{"Device-A"},
		"X-Emby-Device-Id":      []string{"device-a"},
		"X-Emby-Client-Version": []string{"1.0"},
	})

	source, headers := svc.ResolvePassthroughHeaders(http.Header{}, "token-a")
	if source != "captured-token" {
		t.Fatalf("source = %q, want captured-token", source)
	}
	if got := headers.Get("X-Emby-Client"); got != "Client-A" {
		t.Fatalf("client = %q, want Client-A", got)
	}

	live := http.Header{
		"User-Agent":            []string{"Live-UA"},
		"X-Emby-Client":         []string{"Live-Client"},
		"X-Emby-Device-Name":    []string{"Live Device"},
		"X-Emby-Device-Id":      []string{"live-device"},
		"X-Emby-Client-Version": []string{"9.9"},
	}
	source, headers = svc.ResolvePassthroughHeaders(live, "token-a")
	if source != "live-request" {
		t.Fatalf("source = %q, want live-request", source)
	}
	if got := headers.Get("X-Emby-Client"); got != "Live-Client" {
		t.Fatalf("client = %q, want Live-Client", got)
	}

	svc.DeleteCaptured("token-a")
	source, headers = svc.ResolvePassthroughHeaders(http.Header{}, "token-a")
	if source != "infuse-fallback" {
		t.Fatalf("source = %q, want infuse-fallback", source)
	}
	if got := headers.Get("X-Emby-Client"); got != "Infuse" {
		t.Fatalf("fallback client = %q, want Infuse", got)
	}
}
