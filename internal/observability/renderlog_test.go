package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRenderLogConfig_DefaultsNeedNoEnv(t *testing.T) {
	cfg := RenderLogConfigFromEnv(func(string) string { return "" })
	if !cfg.Enabled {
		t.Error("render log must be enabled by default")
	}
	if want := filepath.Join("logs", "pdf-render.jsonl"); cfg.Path != want {
		t.Errorf("default path = %q, want %q (relative, so it lands inside the repo / state dir)", cfg.Path, want)
	}
	if filepath.IsAbs(cfg.Path) {
		t.Error("default path must be relative to the working directory, not absolute")
	}
	if cfg.MaxBytes != 50<<20 || cfg.MaxFiles != 10 {
		t.Errorf("rotation defaults = %d bytes / %d files, want 50MB / 10", cfg.MaxBytes, cfg.MaxFiles)
	}
}

func TestRenderLogConfig_EnvOverride(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k != RenderLogEnvVar {
				t.Fatalf("unexpected env lookup %q", k)
			}
			return v
		}
	}
	if cfg := RenderLogConfigFromEnv(get("/var/log/x/render.jsonl")); cfg.Path != "/var/log/x/render.jsonl" || !cfg.Enabled {
		t.Errorf("path override: %+v", cfg)
	}
	for _, off := range []string{"off", "OFF", "none", "false", "0", " disabled "} {
		if cfg := RenderLogConfigFromEnv(get(off)); cfg.Enabled {
			t.Errorf("%q should disable the render log", off)
		}
	}
}

func TestRotatingFile_CreatesMissingDirectoryAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "nested", "pdf-render.jsonl")
	rf, err := OpenRotatingFile(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	rf.Write([]byte("one\n"))
	rf.Close()

	rf, err = OpenRotatingFile(path, 1<<20, 3) // reopen: must append, not truncate
	if err != nil {
		t.Fatal(err)
	}
	rf.Write([]byte("two\n"))
	rf.Close()

	if got, _ := os.ReadFile(path); string(got) != "one\ntwo\n" {
		t.Errorf("file = %q, want both lines appended across reopen", got)
	}
}

func TestRotatingFile_RotatesBySizeAndKeepsBoundedBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	rf, err := OpenRotatingFile(path, 10, 2) // 10-byte limit, keep 2 backups
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	for _, line := range []string{"aaaaaa\n", "bbbbbb\n", "cccccc\n", "dddddd\n"} { // 7 bytes each
		if _, err := rf.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if got := read(path); got != "dddddd\n" {
		t.Errorf("current = %q, want the newest record only", got)
	}
	if got := read(path + ".1"); got != "cccccc\n" {
		t.Errorf(".1 = %q, want cccccc", got)
	}
	if got := read(path + ".2"); got != "bbbbbb\n" {
		t.Errorf(".2 = %q, want bbbbbb", got)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("a third backup exists: MaxFiles=2 must bound retention (oldest dropped)")
	}
}

func TestRotatingFile_ConcurrentWritesLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	rf, err := OpenRotatingFile(path, 0, 1) // rotation off
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rf.Write([]byte("line\n"))
			}
		}()
	}
	wg.Wait()
	rf.Close()
	if got, _ := os.ReadFile(path); strings.Count(string(got), "line\n") != 1000 {
		t.Errorf("lines = %d, want 1000", strings.Count(string(got), "line\n"))
	}
}

// The renderLogger writes pdf_render JSON to stdout and to the file.
func TestNewRenderLogger_WritesToStdoutAndFile(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderLogConfig{Enabled: true, Path: filepath.Join(dir, "logs", "pdf-render.jsonl"), MaxBytes: 1 << 20, MaxFiles: 2}
	var stdout, warn bytes.Buffer
	l, closer := NewRenderLogger(cfg, slog.LevelInfo, &stdout, slog.New(slog.NewJSONHandler(&warn, nil)))
	l.Info("pdf_render", "engine", Engine)
	closer.Close()

	fileBytes, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	for name, got := range map[string]string{"stdout": stdout.String(), "file": string(fileBytes)} {
		var rec map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(got)), &rec) != nil || rec["msg"] != "pdf_render" {
			t.Errorf("%s = %q, want a pdf_render JSON record", name, got)
		}
	}
	if !strings.Contains(warn.String(), "render log file enabled") {
		t.Errorf("startup should say where the log goes: %s", warn.String())
	}
}

// An unwritable location must never stop the service: warn once, keep stdout.
func TestNewRenderLogger_UnwritableLocationFallsBackToStdout(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := RenderLogConfig{Enabled: true, Path: filepath.Join(blocker, "logs", "pdf-render.jsonl"), MaxBytes: 1 << 20, MaxFiles: 2}

	var stdout, warn bytes.Buffer
	l, closer := NewRenderLogger(cfg, slog.LevelInfo, &stdout, slog.New(slog.NewJSONHandler(&warn, nil)))
	l.Info("pdf_render", "engine", Engine)
	if err := closer.Close(); err != nil {
		t.Errorf("closer must be a harmless no-op, got %v", err)
	}

	if !strings.Contains(stdout.String(), `"pdf_render"`) {
		t.Error("records must still reach stdout when the file is unavailable")
	}
	if !strings.Contains(warn.String(), "render log file unavailable") || !strings.Contains(warn.String(), "WorkingDirectory") {
		t.Errorf("expected one actionable warning, got: %s", warn.String())
	}
}

func TestNewRenderLogger_DisabledWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderLogConfig{Enabled: false, Path: filepath.Join(dir, "logs", "x.jsonl")}
	var stdout bytes.Buffer
	l, closer := NewRenderLogger(cfg, slog.LevelInfo, &stdout, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	l.Info("pdf_render")
	closer.Close()
	if _, err := os.Stat(filepath.Dir(cfg.Path)); err == nil {
		t.Error("disabled render log must not create its directory")
	}
	if !strings.Contains(stdout.String(), "pdf_render") {
		t.Error("stdout must still receive records when the file is disabled")
	}
}
