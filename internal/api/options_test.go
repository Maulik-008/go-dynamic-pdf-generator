package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

func TestParseDimension(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"1in", 1, false},
		{"0.4", 0.4, false}, // bare number = inches
		{"25.4mm", 1, false},
		{"2.54cm", 1, false},
		{"72pt", 1, false},
		{"96px", 1, false},
		{"10mm", 10.0 / 25.4, false},
		{" 0 ", 0, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1in", 0, true},
		{"10furlongs", 0, true},
	}
	for _, c := range cases {
		got, err := ParseDimension(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseDimension(%q): want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDimension(%q): unexpected error %v", c.in, err)
			continue
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ParseDimension(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPaperSizeInches(t *testing.T) {
	if w, h, ok := PaperSizeInches("Letter"); !ok || w != 8.5 || h != 11 {
		t.Errorf("Letter = %v,%v,%v", w, h, ok)
	}
	if w, h, ok := PaperSizeInches(" a4 "); !ok || w != 8.27 || h != 11.69 {
		t.Errorf("a4 = %v,%v,%v", w, h, ok)
	}
	if _, _, ok := PaperSizeInches("nope"); ok {
		t.Error("unknown size reported ok")
	}
}

// resolveJSON decodes an options object and resolves it against base.
func resolveJSON(t *testing.T, body string, base renderengines.RenderOptions) (renderengines.RenderOptions, bool, bool, error) {
	t.Helper()
	var in optionsInput
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		return base, false, false, err
	}
	return in.resolve(base)
}

func TestOptionsResolve_NilLeavesBaseUntouched(t *testing.T) {
	base := renderengines.DefaultRenderOptions()
	var in *optionsInput
	got, embed, fit, err := in.resolve(base)
	if err != nil || embed || fit {
		t.Fatalf("nil options: embed=%v fit=%v err=%v", embed, fit, err)
	}
	if got != base {
		t.Fatalf("nil options changed base: %+v", got)
	}
}

func TestOptionsResolve_Overlay(t *testing.T) {
	base := renderengines.DefaultRenderOptions()
	base.PaperWidth, base.PaperHeight = 8.5, 11 // Letter deployment default
	base.MarginBottom = 10.0 / 25.4

	got, _, _, err := resolveJSON(t, `{
		"landscape": true,
		"scale": 0.9,
		"printBackground": false,
		"waitForImages": false,
		"timeoutMs": 5000,
		"margin": {"top": "1in", "left": 0.25},
		"displayHeaderFooter": true,
		"footerTemplate": "<span></span>"
	}`, base)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Landscape || got.Scale != 0.9 || got.PrintBackground || got.WaitForImages {
		t.Errorf("scalar overlay wrong: %+v", got)
	}
	if got.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", got.Timeout)
	}
	if got.MarginTop != 1 || got.MarginLeft != 0.25 {
		t.Errorf("margin overlay wrong: top=%v left=%v", got.MarginTop, got.MarginLeft)
	}
	// Untouched fields keep the deployment default.
	if math.Abs(got.MarginBottom-10.0/25.4) > 1e-9 {
		t.Errorf("MarginBottom should stay the base value, got %v", got.MarginBottom)
	}
	if got.PaperWidth != 8.5 || got.PaperHeight != 11 {
		t.Errorf("paper should stay Letter, got %v x %v", got.PaperWidth, got.PaperHeight)
	}
	if !got.DisplayHeaderFooter || got.FooterTemplate != "<span></span>" {
		t.Errorf("header/footer overlay wrong: %+v", got)
	}
}

func TestOptionsResolve_PaperSizeAndExplicitDimensions(t *testing.T) {
	base := renderengines.DefaultRenderOptions()

	got, _, _, err := resolveJSON(t, `{"paperSize":"legal"}`, base)
	if err != nil || got.PaperWidth != 8.5 || got.PaperHeight != 14 {
		t.Fatalf("legal: %+v err=%v", got, err)
	}

	got, _, _, err = resolveJSON(t, `{"width":"210mm","height":"297mm"}`, base)
	if err != nil {
		t.Fatalf("explicit dims: %v", err)
	}
	if math.Abs(got.PaperWidth-210.0/25.4) > 1e-9 || math.Abs(got.PaperHeight-297.0/25.4) > 1e-9 {
		t.Fatalf("explicit dims wrong: %v x %v", got.PaperWidth, got.PaperHeight)
	}
}

func TestOptionsResolve_Errors(t *testing.T) {
	base := renderengines.DefaultRenderOptions()
	for _, body := range []string{
		`{"paperSize":"B7"}`,
		`{"scale":5}`,
		`{"scale":0.01}`,
		`{"width":"8.5in"}`, // height missing
		`{"paperSize":"A4","width":"8in","height":"11in"}`, // both
	} {
		if _, _, _, err := resolveJSON(t, body, base); err == nil {
			t.Errorf("%s: expected an error", body)
		}
	}
}

func TestOptionsResolve_TimeoutClamped(t *testing.T) {
	base := renderengines.DefaultRenderOptions()
	got, _, _, _ := resolveJSON(t, `{"timeoutMs": 50}`, base)
	if got.Timeout != time.Duration(minTimeoutMs)*time.Millisecond {
		t.Errorf("under-min timeout = %v, want %dms", got.Timeout, minTimeoutMs)
	}
	got, _, _, _ = resolveJSON(t, `{"timeoutMs": 999999}`, base)
	if got.Timeout != time.Duration(maxTimeoutMs)*time.Millisecond {
		t.Errorf("over-max timeout = %v, want %dms", got.Timeout, maxTimeoutMs)
	}
}

func TestOptionsResolve_Switches(t *testing.T) {
	base := renderengines.DefaultRenderOptions()
	_, embed, fit, err := resolveJSON(t, `{"embedImages":true,"fitToPage":true}`, base)
	if err != nil || !embed || !fit {
		t.Fatalf("embed=%v fit=%v err=%v", embed, fit, err)
	}
}
