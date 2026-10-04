package observability

// The dedicated pdf_render log file: configuration, rotation, and opening it
// without ever being able to take the service down.
//
// Every pdf_render record is always written to stdout (journald /
// `docker logs`) like the rest of the service log. The file is an additional
// copy of just those records, so render timings can be collected and compared
// with the Node service without filtering the whole log. Because stdout always
// has the records, failing to open the file is a warning, never a fatal error.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RenderLogConfig is the one place the render log's defaults live — nothing
// has to be set in .env for it to work.
type RenderLogConfig struct {
	// Enabled turns the dedicated file on. Default true.
	Enabled bool

	// Path is where the file goes. A relative path is resolved against the
	// process working directory: the repo root in development (the repo's
	// logs/ directory carries its own .gitignore), and the service's writable
	// state directory in production, where the systemd unit and the Docker
	// image set the working directory to /var/lib/go-dynamic-pdf-generator.
	Path string

	// MaxBytes rotates the file once it would grow past this size.
	MaxBytes int64

	// MaxFiles is how many rotated files (pdf-render.jsonl.1 … .N) to keep.
	MaxFiles int
}

// DefaultRenderLogConfig returns the built-in defaults.
func DefaultRenderLogConfig() RenderLogConfig {
	return RenderLogConfig{
		Enabled:  true,
		Path:     filepath.Join("logs", "pdf-render.jsonl"),
		MaxBytes: 50 << 20, // matches the Node service
		MaxFiles: 10,
	}
}

// RenderLogEnvVar optionally overrides the default location, or turns the
// file off with "off". It is an escape hatch, not something a normal
// deployment needs to set.
const RenderLogEnvVar = "RENDER_LOG_FILE"

// RenderLogConfigFromEnv starts from DefaultRenderLogConfig and applies the
// optional RENDER_LOG_FILE override.
func RenderLogConfigFromEnv(getenv func(string) string) RenderLogConfig {
	cfg := DefaultRenderLogConfig()
	switch v := strings.TrimSpace(getenv(RenderLogEnvVar)); strings.ToLower(v) {
	case "":
	case "off", "none", "false", "0", "disabled":
		cfg.Enabled = false
	default:
		cfg.Path = v
	}
	return cfg
}

// OpenRenderLog opens the render log file per cfg. It returns (nil, nil) when
// disabled. The directory is created if missing.
func OpenRenderLog(cfg RenderLogConfig) (*RotatingFile, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	return OpenRotatingFile(cfg.Path, cfg.MaxBytes, cfg.MaxFiles)
}

// NewRenderLogger builds the logger that receives pdf_render records: stdout
// always, plus the dedicated file when it could be opened. The returned
// closer releases the file (a no-op when there is none). A file that cannot
// be opened is reported once at startup and the service carries on with
// stdout alone — which still has every record.
func NewRenderLogger(cfg RenderLogConfig, level slog.Level, stdout io.Writer, warn *slog.Logger) (*slog.Logger, io.Closer) {
	opts := &slog.HandlerOptions{Level: level}

	file, err := OpenRenderLog(cfg)
	switch {
	case err != nil:
		warn.Warn("render log file unavailable; pdf_render records go to stdout only",
			"path", cfg.Path, "error", err.Error(),
			"hint", "the working directory must be writable (production: WorkingDirectory=/var/lib/go-dynamic-pdf-generator), or set "+RenderLogEnvVar+" to a writable path, or \"off\"")
		return slog.New(slog.NewJSONHandler(stdout, opts)), nopCloser{}
	case file == nil:
		return slog.New(slog.NewJSONHandler(stdout, opts)), nopCloser{}
	}

	abs, absErr := filepath.Abs(file.Path())
	if absErr != nil {
		abs = file.Path()
	}
	warn.Info("render log file enabled", "path", abs, "max_bytes", cfg.MaxBytes, "max_files", cfg.MaxFiles)
	return slog.New(slog.NewJSONHandler(io.MultiWriter(stdout, file), opts)), file
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// RotatingFile is an append-only file that rotates by size: when a write would
// push it past maxBytes, the current file becomes <path>.1, the previous .1
// becomes .2, and so on up to maxFiles, the oldest being dropped. It is
// stdlib-only on purpose (see the package doc), and safe for concurrent use.
//
// Rotation never loses a record: if it fails part-way, writes simply continue
// on the file that is still open.
type RotatingFile struct {
	path     string
	maxBytes int64
	maxFiles int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotatingFile opens (creating it and its directory if needed) path for
// appending. maxBytes <= 0 disables rotation; maxFiles < 1 keeps one backup.
func OpenRotatingFile(path string, maxBytes int64, maxFiles int) (*RotatingFile, error) {
	if maxFiles < 1 {
		maxFiles = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	f, size, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &RotatingFile{path: path, maxBytes: maxBytes, maxFiles: maxFiles, f: f, size: size}, nil
}

func openAppend(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, 0, fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat log file: %w", err)
	}
	return f, st.Size(), nil
}

// Path returns the file's configured path.
func (r *RotatingFile) Path() string { return r.path }

// Write implements io.Writer.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.maxBytes > 0 && r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts the backups and starts a fresh file. Called with r.mu held.
func (r *RotatingFile) rotate() {
	// Drop the oldest, shift .N-1 -> .N ... .1 -> .2, then current -> .1.
	os.Remove(r.backup(r.maxFiles))
	for i := r.maxFiles - 1; i >= 1; i-- {
		os.Rename(r.backup(i), r.backup(i+1))
	}
	if err := os.Rename(r.path, r.backup(1)); err != nil {
		return // keep appending to the current file rather than lose records
	}
	nf, _, err := openAppend(r.path)
	if err != nil {
		// The old handle still points at the renamed file; keep using it.
		return
	}
	r.f.Close()
	r.f, r.size = nf, 0
}

func (r *RotatingFile) backup(i int) string { return fmt.Sprintf("%s.%d", r.path, i) }

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
