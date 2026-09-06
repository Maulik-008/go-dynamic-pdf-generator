package assets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pngServer(t *testing.T) *httptest.Server {
	t.Helper()
	// 1x1 transparent PNG.
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
		0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/logo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(png)
		case "/huge.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, 5<<20))
		case "/notimage":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>nope</html>"))
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// allowLocalhost is the config used by tests, which necessarily fetch from a
// 127.0.0.1 httptest server.
func allowLocalhost() Config {
	return Config{AllowPrivate: true}
}

func TestEmbed_InlinesRemoteImage(t *testing.T) {
	srv := pngServer(t)
	defer srv.Close()

	html := `<html><body><img src="` + srv.URL + `/logo.png"><p>x</p></body></html>`
	out, stats := Embed(context.Background(), html, allowLocalhost())

	if stats.Embedded != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if strings.Contains(out, srv.URL) {
		t.Fatalf("original URL still present: %s", out)
	}
	if !strings.Contains(out, `src="data:image/png;base64,`) {
		t.Fatalf("expected inlined data URI, got: %s", out)
	}
}

func TestEmbed_SkipsInlineAndRelative(t *testing.T) {
	html := `<img src="data:image/png;base64,AAAA"><img src="/local/rel.png"><img src="logo.png">`
	out, stats := Embed(context.Background(), html, allowLocalhost())
	if out != html {
		t.Fatalf("html changed: %s", out)
	}
	if stats.Skipped != 3 || stats.Total != 0 {
		t.Fatalf("stats = %+v, want 3 skipped / 0 total", stats)
	}
}

func TestEmbed_FailureLeavesURLIntact(t *testing.T) {
	srv := pngServer(t)
	defer srv.Close()

	for _, path := range []string{"/500", "/notimage", "/missing"} {
		html := `<img src="` + srv.URL + path + `">`
		out, stats := Embed(context.Background(), html, allowLocalhost())
		if out != html {
			t.Errorf("%s: url replaced despite failure: %s", path, out)
		}
		if stats.Failed != 1 || stats.Embedded != 0 {
			t.Errorf("%s: stats = %+v", path, stats)
		}
	}
}

func TestEmbed_PerImageSizeCap(t *testing.T) {
	srv := pngServer(t)
	defer srv.Close()

	cfg := allowLocalhost()
	cfg.MaxBytesPerImage = 1 << 10 // 1 KiB, well under /huge.png
	html := `<img src="` + srv.URL + `/huge.png">`
	out, stats := Embed(context.Background(), html, cfg)
	if out != html || stats.Failed != 1 {
		t.Fatalf("oversized image should fail and stay a URL: out changed=%v stats=%+v", out != html, stats)
	}
}

func TestEmbed_SSRFGuardBlocksPrivateByDefault(t *testing.T) {
	srv := pngServer(t) // 127.0.0.1
	defer srv.Close()

	html := `<img src="` + srv.URL + `/logo.png">`
	out, stats := Embed(context.Background(), html, Config{}) // AllowPrivate false
	if out != html || stats.Embedded != 0 {
		t.Fatalf("loopback fetch should be blocked by default: stats=%+v", stats)
	}
	if stats.Failed != 1 {
		t.Fatalf("blocked fetch should count as failed, got %+v", stats)
	}
}

func TestEmbed_HostAllowList(t *testing.T) {
	srv := pngServer(t)
	defer srv.Close()

	cfg := allowLocalhost()
	cfg.AllowedHostSuffixes = []string{".amazonaws.com"}
	html := `<img src="` + srv.URL + `/logo.png">`
	out, stats := Embed(context.Background(), html, cfg)
	if out != html || stats.Skipped != 1 {
		t.Fatalf("host not in allow-list should be skipped: stats=%+v", stats)
	}
}

func TestEmbed_DedupesRepeatedURL(t *testing.T) {
	srv := pngServer(t)
	defer srv.Close()

	u := srv.URL + "/logo.png"
	html := `<img src="` + u + `"><img src='` + u + `'>`
	out, stats := Embed(context.Background(), html, allowLocalhost())
	if stats.Total != 1 || stats.Embedded != 1 {
		t.Fatalf("repeated URL should be fetched once: %+v", stats)
	}
	if strings.Count(out, "data:image/png;base64,") != 2 {
		t.Fatalf("both occurrences should be replaced: %s", out)
	}
}
