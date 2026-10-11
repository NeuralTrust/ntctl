// Package chart locates and loads the neuraltrust-platform chart.
package chart

import (
	"fmt"
	"io"
	"log/slog"

	"helm.sh/helm/v4/pkg/action"
	helmchart "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/registry"
)

// Ref is the public OCI location of the chart. It needs no credentials.
const Ref = "oci://europe-west1-docker.pkg.dev/neuraltrust-app-prod/helm-charts/neuraltrust-platform"

// Supported is the chart range this ntctl release installs when no version is
// pinned.
const Supported = ">=3.0.0 <4.0.0"

// Source says where to load the chart from.
type Source struct {
	// Path is a local chart directory or .tgz. Empty means the public registry.
	Path string
	// Version pins an exact version or constraint. Empty means Supported.
	Version string
}

// Loaded is a chart ready to render or install.
type Loaded struct {
	Chart   helmchart.Charter
	Name    string
	Version string
}

// Load resolves and loads the chart.
func Load(settings *cli.EnvSettings, src Source) (*Loaded, error) {
	path := src.Path
	if path == "" {
		rc, err := RegistryClient(settings)
		if err != nil {
			return nil, err
		}
		locate := action.NewInstall(action.NewConfiguration(action.ConfigurationSetLogger(slog.Default().Handler())))
		locate.SetRegistryClient(rc)
		locate.Version = src.Version
		if locate.Version == "" {
			locate.Version = Supported
		}
		path, err = locate.LocateChart(Ref, settings)
		if err != nil {
			return nil, fmt.Errorf("fetching chart %s (%s): %w", Ref, locate.Version, err)
		}
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading chart %s: %w", path, err)
	}
	acc, err := helmchart.NewAccessor(ch)
	if err != nil {
		return nil, fmt.Errorf("reading chart metadata: %w", err)
	}
	meta := acc.MetadataAsMap()
	if t, _ := meta["Type"].(string); t != "" && t != "application" {
		return nil, fmt.Errorf("%s charts are not installable", t)
	}
	if req := acc.MetaDependencies(); len(req) > 0 {
		if err := action.CheckDependencies(ch, req); err != nil {
			return nil, fmt.Errorf("chart dependencies are missing (run `helm dependency build` on a local chart): %w", err)
		}
	}
	name, _ := meta["Name"].(string)
	version, _ := meta["Version"].(string)
	return &Loaded{Chart: ch, Name: name, Version: version}, nil
}

// Latest returns the newest published chart version in the supported range.
func Latest(settings *cli.EnvSettings) (string, error) {
	rc, err := RegistryClient(settings)
	if err != nil {
		return "", err
	}
	tags, err := rc.Tags(Ref[len("oci://"):])
	if err != nil {
		return "", fmt.Errorf("listing chart versions: %w", err)
	}
	v, err := registry.GetTagMatchingVersionOrConstraint(tags, Supported)
	if err != nil {
		return "", fmt.Errorf("resolving latest chart: %w", err)
	}
	return v, nil
}

// RegistryClient returns an anonymous-capable OCI client that also honours
// any registry login the operator already has.
func RegistryClient(settings *cli.EnvSettings) (*registry.Client, error) {
	rc, err := registry.NewClient(
		registry.ClientOptEnableCache(true),
		registry.ClientOptWriter(io.Discard),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("creating registry client: %w", err)
	}
	return rc, nil
}
