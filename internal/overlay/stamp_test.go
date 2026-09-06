package overlay

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

func TestPageCount(t *testing.T) {
	for _, n := range []int{1, 3, 7} {
		got, err := pageCount(makePDF(t, n))
		if err != nil {
			t.Fatalf("pageCount(%d pages): %v", n, err)
		}
		if got != n {
			t.Errorf("pageCount = %d, want %d", got, n)
		}
	}
}

func TestPageCount_InvalidPDF(t *testing.T) {
	if _, err := pageCount([]byte("not a pdf at all")); err == nil {
		t.Fatal("pageCount on garbage bytes: want error, got nil")
	}
}

func TestStampPages_PreservesPageCountAndValidates(t *testing.T) {
	main := makePDF(t, 4)
	frag := makeFragmentPDF(t, "stamped")

	out, err := stampPages(main, frag, []string{"4"})
	if err != nil {
		t.Fatalf("stampPages: %v", err)
	}

	got, err := pageCount(out)
	if err != nil {
		t.Fatalf("pageCount(out): %v", err)
	}
	if got != 4 {
		t.Fatalf("stamped output has %d pages, want 4 (stamp must not add or drop pages)", got)
	}
	if err := api.Validate(bytes.NewReader(out), pdfConf()); err != nil {
		t.Fatalf("stamped output fails validation: %v", err)
	}
	if len(out) <= len(main) {
		t.Errorf("stamped output (%d bytes) not larger than input (%d bytes) — stamp content missing?", len(out), len(main))
	}
}

func TestStampPages_MultipleSelectedPages(t *testing.T) {
	out, err := stampPages(makePDF(t, 5), makeFragmentPDF(t, "x"), []string{"1", "3-4"})
	if err != nil {
		t.Fatalf("stampPages: %v", err)
	}
	if got, _ := pageCount(out); got != 5 {
		t.Fatalf("output pages = %d, want 5", got)
	}
	if err := api.Validate(bytes.NewReader(out), pdfConf()); err != nil {
		t.Fatalf("validation: %v", err)
	}
}

func TestStampPages_NilSelectionStampsEveryPage(t *testing.T) {
	// nil == "all pages" in pdfcpu; this is the contract resolvePages("all")
	// relies on.
	out, err := stampPages(makePDF(t, 3), makeFragmentPDF(t, "x"), nil)
	if err != nil {
		t.Fatalf("stampPages(nil): %v", err)
	}
	if got, _ := pageCount(out); got != 3 {
		t.Fatalf("output pages = %d, want 3", got)
	}
}

func TestStampPages_InvalidMainPDF(t *testing.T) {
	_, err := stampPages([]byte("%PDF-1.4 broken"), makeFragmentPDF(t, "x"), []string{"1"})
	if err == nil {
		t.Fatal("stampPages with a broken main PDF: want error, got nil")
	}
	if !strings.Contains(err.Error(), "apply stamp") {
		t.Errorf("error should identify the failing step, got: %v", err)
	}
}

func TestResolvePages(t *testing.T) {
	const count = 4
	cases := []struct {
		sel     string
		want    []string
		wantErr bool
	}{
		{"", []string{"4"}, false},
		{"last", []string{"4"}, false},
		{"LAST", []string{"4"}, false},
		{" last ", []string{"4"}, false},
		{"first", []string{"1"}, false},
		{"all", nil, false},
		{"2", []string{"2"}, false},
		{"2-3", []string{"2-3"}, false},
		{"1,3", []string{"1", "3"}, false},
		{"1,3-4", []string{"1", "3-4"}, false},
		{"4", []string{"4"}, false},
		{"5", nil, true},    // past the end
		{"0", nil, true},    // not positive
		{"3-2", nil, true},  // descending
		{"2-9", nil, true},  // range past the end
		{"abc", nil, true},  // not a number
		{"2,,3", nil, true}, // empty part
		{"-1", nil, true},   // pdfcpu-style open range not accepted
		{"l", nil, true},    // pdfcpu-style token not accepted
	}
	for _, c := range cases {
		got, err := resolvePages(c.sel, count)
		if c.wantErr {
			if err == nil {
				t.Errorf("resolvePages(%q): want error, got %v", c.sel, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolvePages(%q): unexpected error %v", c.sel, err)
			continue
		}
		if !equalStrings(got, c.want) {
			t.Errorf("resolvePages(%q) = %v, want %v", c.sel, got, c.want)
		}
	}
}

func TestValidatePages_GrammarOnly(t *testing.T) {
	ok := []string{"", "last", "first", "all", "3", "3-5", "2,4", "1,3-4,7"}
	bad := []string{"abc", "3-2", "0", "l", "even", "3-", "-3", "1,,2"}
	for _, s := range ok {
		if err := ValidatePages(s); err != nil {
			t.Errorf("ValidatePages(%q): want nil, got %v", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidatePages(s); err == nil {
			t.Errorf("ValidatePages(%q): want error, got nil", s)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
