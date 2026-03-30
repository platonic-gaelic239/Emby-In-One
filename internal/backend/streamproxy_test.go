package backend

import (
	"strings"
	"testing"
)

func TestRewriteM3U8UsesRelativeProxyPaths(t *testing.T) {
	input := "#EXTM3U\nsegment1.ts\nhttps://cdn.example/Videos/123/hls1/main/seg.ts?foo=1&api_key=upstream\n"
	output := RewriteM3U8(input, "https://upstream.example/Videos/123/master.m3u8", "proxy-token")

	if !strings.Contains(output, "https://upstream.example/Videos/123/segment1.ts?api_key=proxy-token") {
		t.Fatalf("rewritten output missing absolute segment URL: %s", output)
	}
	if !strings.Contains(output, "https://cdn.example/Videos/123/hls1/main/seg.ts") {
		t.Fatalf("rewritten output missing rewritten absolute path: %s", output)
	}
	if strings.Contains(strings.ToLower(output), "upstream") && strings.Contains(output, "api_key=upstream") {
		t.Fatalf("upstream api_key should have been replaced: %s", output)
	}
}
