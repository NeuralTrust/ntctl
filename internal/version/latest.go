package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ReleasesURL is where ntctl's releases are published.
const ReleasesURL = "https://api.github.com/repos/NeuralTrust/ntctl/releases/latest"

// Latest returns the newest published ntctl release, such as v0.9.0.
func Latest(ctx context.Context, hc *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the release list answered %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("reading the latest release: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("the latest release has no tag")
	}
	return rel.TagName, nil
}

// Outdated reports whether latest is newer than running. A build that is not
// a release (dev, or a commit after a tag, which reads as a pre-release) is
// never outdated: it is a developer's.
func Outdated(running, latest string) bool {
	r, err := semver.StrictNewVersion(strings.TrimPrefix(running, "v"))
	if err != nil || r.Prerelease() != "" {
		return false
	}
	l, err := semver.StrictNewVersion(strings.TrimPrefix(latest, "v"))
	if err != nil {
		return false
	}
	return l.GreaterThan(r)
}
