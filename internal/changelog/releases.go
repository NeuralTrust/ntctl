package changelog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ReleasesURL lists the chart's GitHub Releases, newest first. One page
// covers the last 100 releases, more than any upgrade spans in practice.
const ReleasesURL = "https://api.github.com/repos/NeuralTrust/neuraltrust-platform/releases?per_page=100"

// NotesURL is where an operator reads every release's notes in full.
const NotesURL = "https://github.com/NeuralTrust/neuraltrust-platform/releases"

// Release is one published GitHub Release of the chart.
type Release struct {
	Tag        string `json:"tag_name"`
	Published  string `json:"published_at"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// FetchReleases downloads the chart's published GitHub Releases.
func FetchReleases(ctx context.Context, hc *http.Client) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching releases: HTTP %d from %s", resp.StatusCode, ReleasesURL)
	}
	var out []Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("reading releases: %w", err)
	}
	return out, nil
}

// kind is the release's semver level as the release body states it, such as
// "**patch release** — …".
var kind = regexp.MustCompile(`^\*\*(major|minor|patch) release\*\*`)

// boilerplate sections of a release body: what the image table and install
// commands say, the operator sees elsewhere.
var boilerplate = map[string]bool{"container images": true, "installation": true, "install": true}

// FromReleases returns the entries for releases after from, up to and
// including to, newest first. Drafts, prereleases and tags that are not
// versions are skipped.
func FromReleases(rels []Release, from, to string) ([]Entry, error) {
	lo, hi, err := bounds(from, to)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, r := range rels {
		if r.Draft || r.Prerelease {
			continue
		}
		v, err := semver.NewVersion(r.Tag)
		if err != nil || !v.GreaterThan(lo) || v.GreaterThan(hi) {
			continue
		}
		e := parseRelease(r.Body)
		e.Version = v.String()
		if len(r.Published) >= 10 {
			e.Date = r.Published[:10]
		}
		out = append(out, e)
	}
	sortNewest(out)
	return out, nil
}

// parseRelease splits a release body into its level, its commit subjects
// and any notes written beyond them. The generated "What changed" summary
// only justifies the semver level, so it is not kept as a note.
func parseRelease(body string) Entry {
	var e Entry
	var notes strings.Builder
	section := ""
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "## "):
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			if section != "what changed" && !boilerplate[section] {
				notes.WriteString(line + "\n")
			}
			continue
		case strings.HasPrefix(line, "### "):
			if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "### ")), "commits") {
				section = "commits"
				continue
			}
			if section == "commits" {
				section = "what changed"
			}
		case trimmed == "---":
			continue
		}
		switch {
		case boilerplate[section]:
		case section == "commits":
			if s, ok := strings.CutPrefix(trimmed, "- "); ok {
				// "- 4d1c319 chore: update …": the hash says nothing here.
				if h, rest, found := strings.Cut(s, " "); found && len(h) >= 7 && isHex(h) {
					s = rest
				}
				e.Commits = append(e.Commits, s)
			}
		case section == "what changed" && e.Kind == "" && kind.MatchString(trimmed):
			e.Kind = kind.FindStringSubmatch(trimmed)[1]
		default:
			notes.WriteString(line + "\n")
		}
	}
	e.Body = strings.TrimSpace(notes.String())
	e.Breaking = e.Kind == "major" || attention.MatchString(e.Body)
	return e
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// Merge combines the CHANGELOG's entries with the releases' for the same
// range. A version's hand-written CHANGELOG notes win over its release body;
// the release still supplies its level and commits.
func Merge(changelog, releases []Entry) []Entry {
	byVersion := map[string]*Entry{}
	out := append([]Entry(nil), releases...)
	for i := range out {
		byVersion[out[i].Version] = &out[i]
	}
	for _, c := range changelog {
		r, ok := byVersion[c.Version]
		if !ok {
			out = append(out, c)
			continue
		}
		c.Kind, c.Commits = r.Kind, r.Commits
		if c.Date == "" {
			c.Date = r.Date
		}
		c.Breaking = c.Breaking || r.Breaking
		*r = c
	}
	sortNewest(out)
	return out
}

// Notes returns the release notes between two chart versions, newest first,
// from the CHANGELOG published with to and the chart's GitHub Releases. Either
// source alone is enough; it fails only when both do.
func Notes(ctx context.Context, hc *http.Client, from, to string) ([]Entry, error) {
	var fromLog, fromRels []Entry
	text, logErr := Fetch(ctx, hc, to)
	if logErr == nil {
		var err error
		if fromLog, err = Between(text, from, to); err != nil {
			return nil, err
		}
	}
	rels, relErr := FetchReleases(ctx, hc)
	if relErr == nil {
		var err error
		if fromRels, err = FromReleases(rels, from, to); err != nil {
			return nil, err
		}
	}
	if logErr != nil && relErr != nil {
		return nil, logErr
	}
	return Merge(fromLog, fromRels), nil
}

func bounds(from, to string) (lo, hi *semver.Version, err error) {
	if lo, err = semver.NewVersion(from); err != nil {
		return nil, nil, fmt.Errorf("current version %q: %w", from, err)
	}
	if hi, err = semver.NewVersion(to); err != nil {
		return nil, nil, fmt.Errorf("target version %q: %w", to, err)
	}
	return lo, hi, nil
}

func sortNewest(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, errA := semver.NewVersion(entries[i].Version)
		b, errB := semver.NewVersion(entries[j].Version)
		if errA != nil || errB != nil {
			return entries[i].Version > entries[j].Version
		}
		return a.GreaterThan(b)
	})
}
