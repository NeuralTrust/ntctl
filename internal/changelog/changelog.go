// Package changelog reads the chart's release notes between two versions, so
// an upgrade shows what changes before it is applied. The chart package does
// not carry its CHANGELOG, so the notes come from the public chart repository.
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
	// Upgrade notes section: something to read before upgrading.
	Breaking bool `json:"breaking"`
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
	lo, err := semver.NewVersion(from)
	if err != nil {
		return nil, fmt.Errorf("current version %q: %w", from, err)
	}
	hi, err := semver.NewVersion(to)
	if err != nil {
		return nil, fmt.Errorf("target version %q: %w", to, err)
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

// Print writes the entries, each cut to at most maxLines lines.
func Print(w io.Writer, entries []Entry, maxLines int) {
	for _, e := range entries {
		head := "v" + e.Version
		if e.Date != "" {
			head += " (" + e.Date + ")"
		}
		if e.Breaking {
			head += "  READ BEFORE UPGRADING"
		}
		fmt.Fprintf(w, "  %s\n", head)
		lines := strings.Split(e.Body, "\n")
		for i, l := range lines {
			if i == maxLines {
				fmt.Fprintf(w, "    … %d more lines in the chart CHANGELOG\n", len(lines)-maxLines)
				break
			}
			fmt.Fprintf(w, "    %s\n", l)
		}
	}
}
