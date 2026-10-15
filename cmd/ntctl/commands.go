package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"helm.sh/helm/v4/pkg/action"
	release "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/chart"
	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/manifest"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/report"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
	"github.com/NeuralTrust/ntctl/internal/version"
)

func runVersion(_ context.Context, args []string, out io.Writer) error {
	var o opts
	fs := newFlags("version", "Print the ntctl version and the latest chart version.", out, &o, false)
	if err := parse(fs, args); err != nil {
		return err
	}
	fmt.Fprintf(out, "ntctl %s (commit %s, built %s)\n", version.Version, version.GitCommit, version.BuildDate)
	latest, err := chart.Latest(o.kube.Settings())
	if err != nil {
		latest = "unknown (" + err.Error() + ")"
	}
	fmt.Fprintf(out, "latest chart: %s\n", latest)
	return nil
}

func runRender(ctx context.Context, args []string, out io.Writer) error {
	var o opts
	var dir, kubeVersion string
	fs := newFlags("render", "Print the manifests an install would apply. The output matches `helm template` with the same values, for GitOps.", out, &o, true)
	fs.StringVar(&dir, "output-dir", "", "write values.yaml and manifests.yaml here instead of printing the manifests")
	fs.StringVar(&kubeVersion, "kube-version", "", "Kubernetes version to render for (default: Helm's default)")
	if err := parse(fs, args); err != nil {
		return err
	}
	vals, err := o.effectiveValues(values.Inputs{})
	if err != nil {
		return err
	}
	ch, err := chart.Load(o.kube.Settings(), chart.Source{Path: o.chartPath, Version: o.version})
	if err != nil {
		return err
	}
	rendered, err := helm.Render(ctx, ch, vals, helm.RenderOptions{Release: o.release, Namespace: o.kube.Namespace, KubeVersion: kubeVersion})
	if err != nil {
		return err
	}
	if dir == "" {
		_, err = io.WriteString(out, rendered)
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := values.Save(filepath.Join(dir, "values.yaml"), vals); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte(rendered), 0o600); err != nil {
		return fmt.Errorf("writing manifests: %w", err)
	}
	fmt.Fprintf(out, "Wrote %s/values.yaml and %s/manifests.yaml (chart %s %s)\n", dir, dir, ch.Name, ch.Version)
	return nil
}

func runPreflight(ctx context.Context, args []string, out io.Writer) error {
	var o opts
	var mode string
	fs := newFlags("preflight", "Check a cluster against what an install or upgrade needs. Changes nothing.", out, &o, true)
	fs.StringVar(&mode, "for", "install", "install or upgrade")
	fs.StringVar(&o.output, "output", "text", "text or json")
	if err := parse(fs, args); err != nil {
		return err
	}
	m := preflight.Mode(mode)
	if m != preflight.ForInstall && m != preflight.ForUpgrade {
		return fmt.Errorf("--for must be install or upgrade")
	}
	p, err := o.prepare(ctx, m, nil)
	if err != nil {
		return err
	}
	if err := preflight.Print(out, p.results, o.output == "json"); err != nil {
		return err
	}
	if preflight.Failed(p.results) {
		return errPreflight
	}
	return nil
}

func runStatus(ctx context.Context, args []string, out io.Writer) error {
	var o opts
	fs := newFlags("status", "Show readiness, pods that need attention and the DNS records for an install.", out, &o, false)
	fs.StringVar(&o.output, "output", "text", "text or json")
	if err := parse(fs, args); err != nil {
		return err
	}
	cs, dyn, err := o.clients()
	if err != nil {
		return err
	}
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return err
	}
	rel, err := helm.Current(cfg, o.release)
	if err != nil {
		return err
	}
	st, err := report.Collect(ctx, cs, dyn, o.kube.Namespace)
	if err != nil {
		return err
	}
	if o.output == "json" {
		doc := map[string]any{"release": releaseSummary(rel), "status": st}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	if rel == nil {
		fmt.Fprintf(out, "No release %q in namespace %q.\n\n", o.release, o.kube.Namespace)
	} else {
		fmt.Fprintf(out, "Release %s: chart %s, revision %d, %s, updated %s\n\n",
			rel.Name, rel.Chart.Metadata.Version, rel.Version, rel.Info.Status, rel.Info.LastDeployed.Format(time.RFC3339))
	}
	st.Print(out)
	return nil
}

func releaseSummary(rel *release.Release) map[string]any {
	if rel == nil {
		return nil
	}
	return map[string]any{
		"name": rel.Name, "chart": rel.Chart.Metadata.Version, "revision": rel.Version,
		"status": rel.Info.Status.String(), "updated": rel.Info.LastDeployed,
	}
}

// prepared is everything an install, upgrade or preflight run works from.
type prepared struct {
	cs      kubernetes.Interface
	dyn     dynamic.Interface
	cfg     *action.Configuration
	cluster *detect.Cluster
	current *release.Release
	chart   *chart.Loaded
	// values are what ntctl passes to Helm: the full user values for an
	// install, only the changes for an upgrade.
	values  map[string]any
	results []preflight.Result
}

// prepare reads the cluster, builds the values, renders the chart and runs the
// preflight checks against what the render needs. It changes nothing.
func (o *opts) prepare(ctx context.Context, mode preflight.Mode, willCreate map[string][]string) (*prepared, error) {
	cs, dyn, err := o.clients()
	if err != nil {
		return nil, err
	}
	cluster, err := detect.Inspect(ctx, cs, dyn)
	if err != nil {
		return nil, fmt.Errorf("cannot use the cluster from your kubeconfig: %w", err)
	}
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return nil, err
	}
	current, err := helm.Current(cfg, o.release)
	if err != nil {
		return nil, err
	}
	// Nothing else is worth checking for an upgrade without a release.
	if mode == preflight.ForUpgrade && current == nil {
		return nil, fmt.Errorf("no release %q in namespace %q to upgrade; use `ntctl install`, or pass --namespace/--release", o.release, o.kube.Namespace)
	}
	var defaults values.Inputs
	if mode == preflight.ForInstall {
		defaults = values.Inputs{Platform: cluster.Platform, Products: values.Products}
	}
	vals, err := o.effectiveValues(defaults)
	if err != nil {
		return nil, err
	}
	ch, err := chart.Load(o.kube.Settings(), chart.Source{Path: o.chartPath, Version: o.version})
	if err != nil {
		return nil, err
	}
	renderVals := vals
	if mode == preflight.ForUpgrade && current != nil {
		renderVals = values.Merge(copyMap(current.Config), copyMap(vals))
	}
	if willCreate == nil && mode == preflight.ForInstall {
		willCreate = managedSecrets(renderVals)
	}
	rendered, err := helm.Render(ctx, ch, renderVals, helm.RenderOptions{
		Release: o.release, Namespace: o.kube.Namespace, KubeVersion: cluster.Version, IsUpgrade: mode == preflight.ForUpgrade,
	})
	if err != nil {
		return nil, fmt.Errorf("the chart does not render with these values: %w", err)
	}
	objs, err := manifest.Parse(rendered)
	if err != nil {
		return nil, err
	}
	platform, _ := values.Get(renderVals, "global", "platform").(string)
	provider, _ := values.Get(renderVals, "global", "ingress", "provider").(string)
	nsLabels, err := detect.NamespaceLabels(ctx, cs, o.kube.Namespace)
	if err != nil {
		return nil, err
	}
	results, err := preflight.Run(ctx, preflight.Input{
		Mode: mode, Release: o.release, Namespace: o.kube.Namespace, ReleaseExists: current != nil,
		Cluster: cluster, Needs: manifest.Analyse(objs), Platform: platform, WillCreate: willCreate,
		IngressProvider: provider, NamespaceLabels: nsLabels, IgnoreCapacity: o.ignoreCapacity,
		Secrets: func(ctx context.Context, name string) ([]string, bool, error) {
			return secrets.Keys(ctx, cs, o.kube.Namespace, name)
		},
	})
	if err != nil {
		return nil, err
	}
	return &prepared{cs: cs, dyn: dyn, cfg: cfg, cluster: cluster, current: current, chart: ch, values: vals, results: results}, nil
}

// clients returns the typed and dynamic clients for the selected context.
func (o *opts) clients() (kubernetes.Interface, dynamic.Interface, error) {
	cs, err := o.kube.Clientset()
	if err != nil {
		return nil, nil, err
	}
	dyn, err := o.kube.Dynamic()
	if err != nil {
		return nil, nil, err
	}
	return cs, dyn, nil
}

// managedSecrets are the Secrets `ntctl install` creates for these values:
// the pull secret and the token Secrets of the enabled products.
func managedSecrets(vals map[string]any) map[string][]string {
	out := map[string][]string{secrets.PullSecret: {".dockerconfigjson"}}
	for _, s := range secrets.TokenSpecs(enabledProducts(vals)) {
		out[s.Name] = append(out[s.Name], s.Key)
	}
	return out
}

func enabledProducts(vals map[string]any) []string {
	var out []string
	for _, p := range values.Products {
		if on, _ := values.Get(vals, "global", "products", p).(bool); on {
			out = append(out, p)
		}
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if mm, ok := v.(map[string]any); ok {
			out[k] = copyMap(mm)
			continue
		}
		out[k] = v
	}
	return out
}
