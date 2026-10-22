package changelog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bodies in the shape the chart's release workflow publishes.
const (
	imageOnly = "## What changed\n\n**patch release** — This is a chore-only update to chart versions and image tags.\n\n" +
		"### Commits\n\n- 4d1c319 chore: update chart versions and image tags for trustguard and firewall\n\n---\n\n" +
		"## Container images\n\n| Component | Image |\n|---|---|\n| trustguard | `example.com/trustguard:v0.52.2` |\n\n---\n\n" +
		"## Installation\n\n```bash\n# Zero-config deploy (secrets auto-generated):\nhelm upgrade --install x .\n```\n"
	withFixes = "## What changed\n\n**patch release** — The commit updates dependencies.\n\n" +
		"### Fixes\n\n- In-cluster Redis now starts when persistence is enabled.\n\n" +
		"### Commits\n\n- b722672 chore: update chart dependencies\n\n---\n\n## Container images\n\n| a | b |\n"
	minor = "## What changed\n\n**minor release** — Adds opt-in operational telemetry.\n\n### Commits\n\n- 0a1b2c3 feat: telemetry\n"
)

func releases() []Release {
	return []Release{
		{Tag: "v3.5.10", Published: "2026-10-22T09:32:08Z", Body: imageOnly},
		{Tag: "v3.5.9", Published: "2026-10-22T09:05:53Z", Body: imageOnly},
		{Tag: "v3.5.8", Published: "2026-10-21T18:00:00Z", Body: imageOnly},
		{Tag: "v3.5.7", Published: "2026-10-21T12:00:00Z", Body: imageOnly},
		{Tag: "v3.5.6", Published: "2026-10-21T08:21:08Z", Body: withFixes},
		{Tag: "v3.5.5", Published: "2026-10-20T08:00:00Z", Body: imageOnly},
		{Tag: "v3.5.0", Published: "2026-10-16T08:00:00Z", Body: minor},
		{Tag: "v3.6.0-rc.1", Prerelease: true, Body: minor},
	}
}

func TestParseRelease(t *testing.T) {
	e := parseRelease(imageOnly)
	if e.Kind != "patch" || e.Body != "" || e.noted() {
		t.Errorf("image-only release = %+v, want a patch with no notes", e)
	}
	if len(e.Commits) != 1 || e.Commits[0] != "chore: update chart versions and image tags for trustguard and firewall" {
		t.Errorf("commits = %q", e.Commits)
	}
	e = parseRelease(withFixes)
	if e.Body != "### Fixes\n\n- In-cluster Redis now starts when persistence is enabled." || !e.noted() {
		t.Errorf("release with fixes = %+v", e)
	}
	if e = parseRelease(minor); !e.noted() || e.Breaking {
		t.Errorf("a minor release has notes to read but is not breaking: %+v", e)
	}
}

func TestFromReleasesRange(t *testing.T) {
	got, err := FromReleases(releases(), "3.5.5", "3.5.10")
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, e := range got {
		versions = append(versions, e.Version)
	}
	if strings.Join(versions, " ") != "3.5.10 3.5.9 3.5.8 3.5.7 3.5.6" {
		t.Fatalf("versions = %v", versions)
	}
	if got[0].Date != "2026-10-22" {
		t.Errorf("date = %q", got[0].Date)
	}
}

func TestMergePrefersTheChangelog(t *testing.T) {
	log := []Entry{{Version: "3.5.9", Date: "2026-10-22", Body: "### Changed\n\n- **Hosted firewall.**"}}
	rels, _ := FromReleases(releases(), "3.5.5", "3.5.10")
	got := Merge(log, rels)
	if len(got) != 5 || got[1].Version != "3.5.9" {
		t.Fatalf("merged = %+v", got)
	}
	if !strings.Contains(got[1].Body, "Hosted firewall") || got[1].Kind != "patch" || len(got[1].Commits) != 1 {
		t.Errorf("3.5.9 = %+v, want the changelog body with the release's kind and commits", got[1])
	}
}

func TestPrintFoldsReleasesWithoutNotes(t *testing.T) {
	rels, _ := FromReleases(releases(), "3.5.5", "3.5.10")
	var out bytes.Buffer
	Print(&out, rels, false)
	s := out.String()
	for _, want := range []string{"v3.5.6 (2026-10-21)\n    ### Fixes", "4 releases have no notes beyond their commits: v3.5.10, v3.5.9, v3.5.8, v3.5.7."} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "chore:") {
		t.Errorf("commits are only printed in full:\n%s", s)
	}
	out.Reset()
	Print(&out, rels, true)
	if !strings.Contains(out.String(), "v3.5.10 (2026-10-22)\n    - chore: update chart versions") {
		t.Errorf("full output lacks the commits:\n%s", out.String())
	}
}

func TestPrintNamesALongRunAsARange(t *testing.T) {
	var entries []Entry
	for _, v := range []string{"3.0.9", "3.0.8", "3.0.7", "3.0.6", "3.0.5", "3.0.4", "3.0.3"} {
		entries = append(entries, Entry{Version: v})
	}
	var out bytes.Buffer
	Print(&out, entries, false)
	if !strings.Contains(out.String(), "7 releases have no notes beyond their commits: between v3.0.3 and v3.0.9.") {
		t.Errorf("output = %q", out.String())
	}
}

func TestNotesFallsBackToEitherSource(t *testing.T) {
	serve := func(log, api bool) *http.Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case log && r.URL.Path == "/v3.1.0/CHANGELOG.md":
				_, _ = w.Write([]byte(sample))
			case api && r.URL.Path == "/releases":
				_ = json.NewEncoder(w).Encode([]Release{{Tag: "v3.1.0", Body: imageOnly}, {Tag: "v3.0.3", Body: imageOnly}})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return &http.Client{Transport: rewrite{srv.URL}}
	}
	ctx := context.Background()
	got, err := Notes(ctx, serve(true, true), "3.0.1", "3.1.0")
	if err != nil || len(got) != 2 || got[0].Kind != "patch" || !got[0].Breaking {
		t.Fatalf("both sources = %+v, %v", got, err)
	}
	if got, err := Notes(ctx, serve(false, true), "3.0.1", "3.1.0"); err != nil || len(got) != 2 {
		t.Fatalf("releases only = %+v, %v", got, err)
	}
	if got, err := Notes(ctx, serve(true, false), "3.0.1", "3.1.0"); err != nil || len(got) != 2 {
		t.Fatalf("changelog only = %+v, %v", got, err)
	}
	if _, err := Notes(ctx, serve(false, false), "3.0.1", "3.1.0"); err == nil {
		t.Fatal("no source at all should be an error")
	}
}
