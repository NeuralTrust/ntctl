// Package changelog reads the chart's release notes between two versions, so
// an upgrade shows what changes before it is applied. The chart package does
// not carry its CHANGELOG, so the notes come from the public chart repository:
// its CHANGELOG, written for releases that change behaviour, and its GitHub
// Releases, published for every version.
package changelog

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// URL is where the CHANGELOG of a chart release is published; %s is the
// release tag.
const URL = "https://raw.githubusercontent.com/NeuralTrust/neuraltrust-platform/%s/CHANGELOG.md"

// Entry is one release's notes.
type Entry struct {
	Version string `json:"version"`
	Date    string `json:"date,omitempty"`
	Body    string `json:"body"`
	// Breaking is set when the notes have a Breaking, Removed, Migration or
	// Upgrade notes section, or the release is a major one: something to read
	// before upgrading.
	Breaking bool `json:"breaking"`
	// Kind is the release's level (major, minor or patch), when known.
	Kind string `json:"kind,omitempty"`
	// Commits are the subjects of the commits the release shipped.
	Commits []string `json:"commits,omitempty"`
}

// noted reports whether the release has notes worth reading before
// upgrading, beyond its commit subjects: a version bump of images alone has
// none.
func (e Entry) noted() bool {
	return e.Body != "" || e.Kind == "major" || e.Kind == "minor"
}

var header = regexp.MustCompile(`^## \[v?([0-9][^\]]*)\]\s*(?:[—–-]\s*(\S+))?`)

var attention = regexp.MustCompile(`(?mi)^### (breaking|removed|migration notes|upgrade notes)`)

// Fetch downloads the CHANGELOG published with a chart version.
func Fetch(ctx context.Context, hc *http.Client, version string) (string, error) {
	u := fmt.Sprintf(URL, "v"+strings.TrimPrefix(version, "v"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching release notes: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching release notes: HTTP %d from %s", resp.StatusCode, u)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("reading release notes: %w", err)
	}
	return string(b), nil
}

// Between returns the entries for versions after from, up to and including
// to, newest first. Unreleased notes and unparsable versions are skipped.
func Between(text, from, to string) ([]Entry, error) {
	lo, hi, err := bounds(from, to)
	if err != nil {
		return nil, err
	}
	var out []Entry
	var cur *Entry
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(body.String())
			cur.Breaking = attention.MatchString(cur.Body)
			out = append(out, *cur)
		}
		cur = nil
		body.Reset()
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			flush()
			m := header.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			v, err := semver.NewVersion(m[1])
			if err != nil || !v.GreaterThan(lo) || v.GreaterThan(hi) {
				continue
			}
			cur = &Entry{Version: v.String(), Date: m[2]}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return out, sc.Err()
}

// maxLines bounds each release's notes unless all are asked for.
const maxLines = 20

// maxListed is how many versions without notes are named one by one.
const maxListed = 6

// maxDetailed is how many releases' notes are written out before the rest are
// cut to one line per change.
const maxDetailed = 3

// maxHeadline bounds a change's one-line form.
const maxHeadline = 110

// Print writes the releases that have notes, each cut to maxLines lines, and
// names the others on one line, so a range of many image-only releases stays
// short. Past maxDetailed releases with notes, each change is cut to its
// headline, except in releases to read before upgrading. With full, every
// release is written whole, with its commits.
func Print(w io.Writer, entries []Entry, full bool) {
	noted := 0
	for _, e := range entries {
		if e.noted() {
			noted++
		}
	}
	var quiet []string
	for _, e := range entries {
		if !full && !e.noted() {
			quiet = append(quiet, "v"+e.Version)
			continue
		}
		head := "v" + e.Version
		if e.Date != "" {
			head += " (" + e.Date + ")"
		}
		if e.Breaking {
			head += "  READ BEFORE UPGRADING"
		}
		fmt.Fprintf(w, "  %s\n", head)
		if !full && !e.Breaking && noted > maxDetailed {
			for _, h := range headlines(e.Body) {
				fmt.Fprintf(w, "    %s\n", h)
			}
			continue
		}
		if e.Body != "" {
			lines := strings.Split(e.Body, "\n")
			for i, l := range lines {
				if i == maxLines && !full {
					fmt.Fprintf(w, "    … %d more lines in the release notes\n", len(lines)-maxLines)
					break
				}
				fmt.Fprintf(w, "    %s\n", l)
			}
		}
		if full {
			for _, c := range e.Commits {
				fmt.Fprintf(w, "    - %s\n", c)
			}
		}
	}
	if len(quiet) == 0 {
		return
	}
	names := strings.Join(quiet, ", ")
	if len(quiet) > maxListed {
		names = "between " + quiet[len(quiet)-1] + " and " + quiet[0]
	}
	what := "release has"
	if len(quiet) > 1 {
		what = "releases have"
	}
	fmt.Fprintf(w, "  %d %s no notes beyond their commits: %s.\n", len(quiet), what, names)
}

// headlines cuts notes to one line per change: the section, then the bold
// title a CHANGELOG bullet opens with, or else the bullet's first sentence.
func headlines(body string) []string {
	var out []string
	section, item := "", ""
	flush := func() {
		if item == "" {
			return
		}
		h := item
		if t, ok := strings.CutPrefix(h, "**"); ok {
			if i := strings.Index(t, "**"); i > 0 {
				h = t[:i]
			}
		} else if i := strings.Index(h, ". "); i > 0 {
			h = h[:i+1]
		}
		if r := []rune(h); len(r) > maxHeadline {
			h = string(r[:maxHeadline-1]) + "…"
		}
		if section != "" {
			h = section + ": " + h
		}
		out = append(out, h)
		item = ""
	}
	for _, l := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(l, "### "):
			flush()
			section = strings.TrimSpace(strings.TrimPrefix(l, "### "))
		case strings.HasPrefix(l, "- "):
			flush()
			item = strings.TrimPrefix(l, "- ")
		case item != "" && strings.HasPrefix(l, "  "):
			// A wrapped line of the same bullet.
			item += " " + strings.TrimSpace(l)
		default:
			flush()
		}
	}
	flush()
	return out
}
