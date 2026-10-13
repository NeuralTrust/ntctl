// Package config keeps ntctl's defaults between runs: which NeuralTrust
// console it talks to and which organization it acts on, the way `gcloud
// config` keeps a project.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Regions maps each NeuralTrust region to its console. Each region is a
// separate console with its own accounts.
var Regions = map[string]string{
	"eu": "https://app.neuraltrust.ai",
	"us": "https://app.us.neuraltrust.ai",
}

// DefaultRegion is used until one is chosen.
const DefaultRegion = "eu"

// Config is what `ntctl config` sets.
type Config struct {
	// AppURL is the console signed in to; Region is its region, which the
	// install passes to the chart as global.saasRegion.
	AppURL  string `json:"app_url,omitempty"`
	Region  string `json:"region,omitempty"`
	OrgID   string `json:"org_id,omitempty"`
	OrgName string `json:"org_name,omitempty"`
}

const file = "config.json"

// RegionNames lists the regions, sorted.
func RegionNames() []string {
	out := make([]string, 0, len(Regions))
	for r := range Regions {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// AppURL returns the console of a region.
func AppURL(region string) (string, error) {
	u, ok := Regions[strings.ToLower(region)]
	if !ok {
		return "", fmt.Errorf("unknown region %q (valid: %s)", region, strings.Join(RegionNames(), ", "))
	}
	return u, nil
}

// Load returns the saved config, or an empty one when nothing is saved.
func Load() (*Config, error) {
	c := &Config{}
	if err := ReadJSON(file, c); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return c, nil
}

// Save writes the config.
func (c *Config) Save() error {
	return WriteJSON(file, c)
}

// Dir is where ntctl keeps its files: $NTCTL_CONFIG_DIR, else
// $XDG_CONFIG_HOME/ntctl, else ~/.config/ntctl.
func Dir() (string, error) {
	if d := os.Getenv("NTCTL_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "ntctl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, ".config", "ntctl"), nil
}

// ReadJSON decodes a file from Dir into v. A missing file returns an error
// matching fs.ErrNotExist.
func ReadJSON(name string, v any) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- a fixed name under ntctl's own directory
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Join(dir, name), err)
	}
	return nil
}

// WriteJSON writes v to a file in Dir, readable only by the user. The file is
// replaced in one step, so a crash never leaves it half written.
func WriteJSON(name string, v any) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename; only cleans up a failed write
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close() // the write error is the one reported
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}
