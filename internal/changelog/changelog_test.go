package changelog

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sample = `# Changelog

## [Unreleased]

### Fixed

- not released yet

## [v3.1.0] — 2026-11-02

### Removed

- **an old key.** Gone.

## [v3.0.3] — 2026-10-13

### Fixed

- **a fix.** Line one.
  Line two.

## [v3.0.1] — 2026-10-09

### Added

- **a feature.**

## [v3.0.0] — 2026-10-07

### Removed

- base release
`

func TestBetween(t *testing.T) {
	got, err := Between(sample, "3.0.1", "3.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Version != "3.1.0" || got[1].Version != "3.0.3" {
		t.Fatalf("entries = %+v, want 3.1.0 and 3.0.3 only", got)
	}
	if !got[0].Breaking || got[1].Breaking {
		t.Errorf("only the release with a Removed section is flagged: %+v", got)
	}
	if got[1].Date != "2026-10-13" || !strings.Contains(got[1].Body, "Line two.") {
		t.Errorf("entry = %+v", got[1])
	}
	if strings.Contains(got[0].Body, "not released yet") {
		t.Error("Unreleased notes must not be shown")
	}
}

func TestBetweenSameVersion(t *testing.T) {
	got, err := Between(sample, "3.0.3", "3.0.3")
	if err != nil || len(got) != 0 {
		t.Fatalf("no entries expected, got %+v, %v", got, err)
	}
}

func TestPrintCutsLongEntries(t *testing.T) {
	var out bytes.Buffer
	entries, _ := Between(sample, "3.0.1", "3.1.0")
	Print(&out, entries, 2)
	for _, want := range []string{"v3.1.0 (2026-11-02)  READ BEFORE UPGRADING", "more lines in the chart CHANGELOG"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3.0.3/CHANGELOG.md" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()
	hc := &http.Client{Transport: rewrite{srv.URL}}
	got, err := Fetch(context.Background(), hc, "3.0.3")
	if err != nil || !strings.Contains(got, "## [v3.0.3]") {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	if _, err := Fetch(context.Background(), hc, "9.9.9"); err == nil {
		t.Fatal("a missing tag should be an error")
	}
}

// rewrite sends every request to the test server, keeping the path after the
// repository name.
type rewrite struct{ base string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path[strings.Index(req.URL.Path, "/v"):]
	out, err := http.NewRequestWithContext(req.Context(), req.Method, r.base+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(out)
}
