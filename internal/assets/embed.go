// Package assets ports pdf-service-saas's embedImagesAsBase64: an opt-in
// pass that rewrites remote <img src> references in an HTML document to
// self-contained data: URIs by fetching each image once, server-side,
// before the document is handed to Chromium.
//
// Why it exists: documents whose images are remote (signed object-storage
// URLs, a clinic logo on a CDN) make the render block on — and fail
// because of — a slow or briefly-unavailable URL. Inlining removes that
// dependency from the render's critical path. It is never on by default:
// fetching URLs named in a submitted document is server-side request
// forgery surface, so a request must ask for it (options.embedImages) AND
// the deployment must allow it (EMBED_IMAGES_ENABLED), and even then a
// resolved private/loopback/link-local address is refused unless explicitly
// permitted.
package assets

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config tunes an Embed run. The zero value is usable; every field has a
// sensible default applied by withDefaults.
type Config struct {
	// PerImageTimeout bounds a single image fetch. Default 8s (matches
	// pdf-service-saas).
	PerImageTimeout time.Duration
	// MaxBytesPerImage rejects an individual image larger than this.
	// Default 10 MiB.
	MaxBytesPerImage int64
	// MaxTotalBytes caps the sum of all embedded images for one document;
	// once exceeded, remaining images are left as URLs. Default 32 MiB.
	MaxTotalBytes int64
	// MaxConcurrency bounds parallel fetches. Default 8.
	MaxConcurrency int
	// AllowPrivate permits fetching URLs that resolve to loopback, private,
	// or link-local addresses. Default false. Leave false in production.
	AllowPrivate bool
	// AllowedHostSuffixes, when non-empty, restricts fetches to hosts with
	// one of these suffixes (e.g. ".amazonaws.com"). Empty = any public
	// host.
	AllowedHostSuffixes []string
}

func (c Config) withDefaults() Config {
	if c.PerImageTimeout <= 0 {
		c.PerImageTimeout = 8 * time.Second
	}
	if c.MaxBytesPerImage <= 0 {
		c.MaxBytesPerImage = 10 << 20
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = 32 << 20
	}
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 8
	}
	return c
}

// Stats is a per-document summary of what Embed did, for logging.
type Stats struct {
	Total    int // distinct remote URLs found
	Embedded int
	Failed   int
	Skipped  int // non-http(s), data:, or disallowed host
	Bytes    int64
}

// imgSrcRe captures the URL inside src="..." / src='...' of an <img> tag.
// Mirrors pdf-service-saas's regex-based approach — a full HTML parse is
// unnecessary for this and would change which malformed markup is matched.
var imgSrcRe = regexp.MustCompile(`(?i)<img\b[^>]*?\bsrc\s*=\s*("([^"]*)"|'([^']*)')`)

// Embed returns html with every fetchable remote <img src> replaced by a
// data: URI, plus a Stats summary. A fetch that fails for any reason leaves
// that image's original URL in place — a missing image must never fail the
// whole render (same policy as pdf-service-saas's skipOnError default).
func Embed(ctx context.Context, html string, cfg Config) (string, Stats) {
	cfg = cfg.withDefaults()

	urls := distinctImageURLs(html)
	var stats Stats
	if len(urls) == 0 {
		return html, stats
	}

	client := newImageClient(cfg)
	replacements := make(map[string]string, len(urls))
	var mu sync.Mutex
	var totalBytes int64

	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup
	for _, u := range urls {
		if reason := skipReason(u, cfg); reason != "" {
			mu.Lock()
			stats.Skipped++
			mu.Unlock()
			continue
		}
		mu.Lock()
		stats.Total++
		mu.Unlock()

		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()

			mu.Lock()
			overBudget := totalBytes >= cfg.MaxTotalBytes
			mu.Unlock()
			if overBudget {
				mu.Lock()
				stats.Failed++
				mu.Unlock()
				return
			}

			dataURI, n, err := fetchAsDataURI(ctx, client, u, cfg.MaxBytesPerImage)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || totalBytes+n > cfg.MaxTotalBytes {
				stats.Failed++
				return
			}
			replacements[u] = dataURI
			totalBytes += n
			stats.Embedded++
			stats.Bytes += n
		}(u)
	}
	wg.Wait()

	out := html
	for u, dataURI := range replacements {
		out = strings.ReplaceAll(out, u, dataURI)
	}
	return out, stats
}

func distinctImageURLs(html string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range imgSrcRe.FindAllStringSubmatch(html, -1) {
		u := m[2]
		if u == "" {
			u = m[3]
		}
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// skipReason returns a non-empty string when u should not be fetched at all
// (so it stays in the document untouched), or "" when it is a candidate.
func skipReason(u string, cfg Config) string {
	low := strings.ToLower(u)
	if strings.HasPrefix(low, "data:") {
		return "already inline"
	}
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		return "not an absolute http(s) url"
	}
	if len(cfg.AllowedHostSuffixes) > 0 {
		host := hostOf(u)
		for _, s := range cfg.AllowedHostSuffixes {
			if strings.HasSuffix(host, strings.ToLower(s)) {
				return ""
			}
		}
		return "host not in allow-list"
	}
	return ""
}

func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	return strings.ToLower(s)
}

// newImageClient builds an http.Client whose dialer refuses to connect to a
// disallowed address. The check is in Dialer.Control, which runs after DNS
// resolution on the actual IP about to be dialed, so it also defeats DNS
// rebinding (a hostname that resolves public on the first lookup and private
// on the connect).
func newImageClient(cfg Config) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !cfg.AllowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isDisallowedIP(ip) {
				return fmt.Errorf("blocked non-public address %s", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: cfg.PerImageTimeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConnsPerHost: cfg.MaxConcurrency,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if s := strings.ToLower(req.URL.Scheme); s != "http" && s != "https" {
				return fmt.Errorf("refusing redirect to %s scheme", s)
			}
			return nil
		},
	}
}

func isDisallowedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// 100.64.0.0/10 — carrier-grade NAT, not covered by IsPrivate.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

func fetchAsDataURI(ctx context.Context, client *http.Client, url string, maxBytes int64) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", 0, err
	}
	if int64(len(body)) > maxBytes {
		return "", 0, fmt.Errorf("image exceeds %d bytes", maxBytes)
	}

	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	if ct == "" {
		ct = mimeFromExt(url)
	}
	if !strings.HasPrefix(ct, "image/") {
		return "", 0, fmt.Errorf("not an image (%s)", ct)
	}

	var sb strings.Builder
	sb.WriteString("data:")
	sb.WriteString(ct)
	sb.WriteString(";base64,")
	sb.WriteString(base64.StdEncoding.EncodeToString(body))
	return sb.String(), int64(len(body)), nil
}

func mimeFromExt(url string) string {
	u := url
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	dot := strings.LastIndexByte(u, '.')
	if dot < 0 {
		return "image/png"
	}
	switch strings.ToLower(u[dot+1:]) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}
