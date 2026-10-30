package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"helm.sh/helm/v4/pkg/action"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/NeuralTrust/ntctl/internal/chart"
	"github.com/NeuralTrust/ntctl/internal/generator"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/manifest"
	"github.com/NeuralTrust/ntctl/internal/values"
)

const secretsSummary = `Make the Secrets the chart's secrets generator would: platform-secrets,
with every generated credential of the install, and the TLS Secrets it issues.

The generator is a Job the chart runs as a pre-install and pre-upgrade hook.
Helm, ntctl, Argo CD and Flux run it, and an install needs nothing else. These
commands are for installs that cannot run it, or that keep their credentials
in a vault:

  create  creates them in the namespace: missing Secrets and missing keys only,
          never a value that is already there. For a GitOps tool that skips
          Helm hooks, run it before the first sync.
  print   prints them, real values included, as Secret manifests (--output
          yaml) or as {secret: {key: value}} (--output json) for Vault or the
          External Secrets Operator. A Secret that operator creates is
          checked by the generator, never written. Nothing is changed.

Both render the chart with your values, so the keys follow the products and
install shape the chart runs. They read what the namespace already holds:
values in use are kept, values in Secrets earlier chart versions rendered are
adopted, and only what is missing is generated. print --offline skips the
cluster and generates everything, for a namespace that does not exist yet.`

func runSecrets(ctx context.Context, args []string, out io.Writer) error {
	sub, rest := subcommand(args)
	if sub != "create" && sub != "print" {
		return usageError("secrets", "create|print", secretsSummary, out, sub)
	}
	var o opts
	var offline bool
	fs := newFlags("secrets "+sub, secretsSummary, out, &o, true)
	if sub == "print" {
		fs.StringVar(&o.output, "output", "yaml", "yaml (Secret manifests) or json ({secret: {key: value}})")
		fs.BoolVar(&offline, "offline", false, "do not read the cluster: generate every value, for a namespace that does not exist yet")
	}
	if err := parse(fs, rest); err != nil {
		return err
	}
	if o.output != "" && o.output != "yaml" && o.output != "json" {
		return fmt.Errorf("--output must be yaml or json, not %q", o.output)
	}
	var cs kubernetes.Interface
	if !offline {
		var err error
		if cs, err = o.kube.Clientset(); err != nil {
			return err
		}
	}
	plan, err := o.generatorPlan(ctx, cs)
	if err != nil {
		return err
	}
	if sub == "print" {
		return printPlan(out, plan, o.kube.Namespace, o.output)
	}
	return createPlan(ctx, out, cs, o.kube.Namespace, plan)
}

// generatorPlan renders the chart with the install's values, as a fresh
// install so the generator renders whatever an upgrade would leave it at,
// and works out what it would write into the namespace. A release already in
// the namespace lends its values, under any -f files and flags. cs nil is an
// empty namespace.
func (o *opts) generatorPlan(ctx context.Context, cs kubernetes.Interface) (*generator.Plan, error) {
	vals, err := o.effectiveValues(values.Inputs{})
	if err != nil {
		return nil, err
	}
	var lookup generator.Lookup
	if cs != nil {
		cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
		if err != nil {
			return nil, err
		}
		rel, err := helm.Current(cfg, o.release)
		if err != nil {
			return nil, err
		}
		if rel != nil {
			vals = reuse(rel.Config, vals)
		}
		lookup = func(ctx context.Context, name string) (*corev1.Secret, error) {
			s, err := cs.CoreV1().Secrets(o.kube.Namespace).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return s, err
		}
	}
	ch, err := chart.Load(o.kube.Settings(), chart.Source{Path: o.chartPath, Version: o.version})
	if err != nil {
		return nil, err
	}
	rendered, err := helm.Render(ctx, ch, vals, helm.RenderOptions{Release: o.release, Namespace: o.kube.Namespace})
	if err != nil {
		return nil, fmt.Errorf("the chart does not render with these values: %w", err)
	}
	objs, err := manifest.Parse(rendered)
	if err != nil {
		return nil, err
	}
	plan, err := generator.Build(ctx, objs, lookup)
	if errors.Is(err, generator.ErrNoGenerator) {
		return nil, fmt.Errorf("chart %s runs no secrets generator for these values (a %s install%s), so it renders its Secrets itself and needs none made beforehand",
			ch.Version, values.DeploymentMode(vals), optedOut(vals))
	}
	return plan, err
}

func optedOut(vals map[string]any) string {
	if values.Get(vals, "global", "secrets", "generator", "enabled") == false {
		return ", with global.secrets.generator.enabled false"
	}
	return ""
}

// printPlan writes the plan's Secrets with their values. A Secret the
// operator supplies is left out; what it lacks is said instead.
func printPlan(out io.Writer, plan *generator.Plan, ns, format string) error {
	flat := map[string]map[string]string{}
	var docs []string
	var lacking []string
	for _, s := range plan.Secrets {
		if s.Owner == generator.External {
			if len(s.Missing) > 0 {
				lacking = append(lacking, fmt.Sprintf("%s (yours) lacks %s", s.Name, strings.Join(s.Missing, ", ")))
			}
			continue
		}
		flat[s.Name] = s.Data
		b, err := yaml.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": string(s.Type),
			"metadata":   map[string]any{"name": s.Name, "namespace": ns},
			"stringData": s.Data,
		})
		if err != nil {
			return fmt.Errorf("encoding %s: %w", s.Name, err)
		}
		docs = append(docs, string(b))
	}
	if format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(flat); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "# Real credentials: keep this output out of version control and logs.")
		for _, d := range docs {
			fmt.Fprintf(out, "---\n%s", d)
		}
	}
	if len(lacking) > 0 {
		return fmt.Errorf("the generator would stop on Secrets you supply: %s", strings.Join(lacking, "; "))
	}
	return nil
}

// createPlan makes what the plan holds and the namespace lacks: absent
// Secrets with the generator's own metadata, so its next run treats them as
// its own, and missing keys in Secrets it already manages. It never changes
// a value, and never writes an operator's Secret.
func createPlan(ctx context.Context, out io.Writer, cs kubernetes.Interface, ns string, plan *generator.Plan) error {
	var problems []string
	for _, s := range plan.Secrets {
		switch {
		case s.Owner == generator.External && len(s.Missing) > 0:
			problems = append(problems, fmt.Sprintf("%s is yours and lacks %s; add them, the generator does not write it", s.Name, strings.Join(s.Missing, ", ")))
		case s.Owner == generator.External:
			fmt.Fprintf(out, "Secret %s: yours, holds every key\n", s.Name)
		case s.Owner == generator.Absent:
			if err := createSecret(ctx, cs, ns, s); err != nil {
				return err
			}
			fmt.Fprintf(out, "Secret %s: created with %d keys\n", s.Name, len(s.Data))
		case s.Type == corev1.SecretTypeTLS:
			// A certificate is a unit: one that is partly missing is not
			// patched together from two issues.
			whole, err := completeTLS(ctx, cs, ns, s.Name)
			if err != nil {
				return err
			}
			if !whole {
				problems = append(problems, fmt.Sprintf("%s holds an incomplete certificate; delete it and run this again to issue a new one", s.Name))
				continue
			}
			fmt.Fprintf(out, "Secret %s: complete, unchanged\n", s.Name)
		default:
			added, err := addKeys(ctx, cs, ns, s)
			if err != nil {
				return err
			}
			if len(added) > 0 {
				fmt.Fprintf(out, "Secret %s: added %s\n", s.Name, strings.Join(added, ", "))
			} else {
				fmt.Fprintf(out, "Secret %s: complete, unchanged\n", s.Name)
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func createSecret(ctx context.Context, cs kubernetes.Interface, ns string, s generator.Secret) error {
	ann := map[string]string{
		generator.Marker:                     "managed",
		"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
		"argocd.argoproj.io/sync-options":    "Prune=false,Delete=false",
	}
	for k, v := range s.Annotations {
		ann[k] = v
	}
	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: ns, Annotations: ann, Labels: map[string]string{
			"app.kubernetes.io/part-of": "neuraltrust-platform", "app.kubernetes.io/managed-by": generator.ManagedBy,
		}},
		Type: s.Type, Data: map[string][]byte{},
	}
	for k, v := range s.Data {
		obj.Data[k] = []byte(v)
	}
	if _, err := cs.CoreV1().Secrets(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating Secret %s: %w", s.Name, err)
	}
	return nil
}

// completeTLS reports whether a live TLS Secret holds a certificate and key.
func completeTLS(ctx context.Context, cs kubernetes.Interface, ns, name string) (bool, error) {
	live, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading Secret %s: %w", name, err)
	}
	return len(live.Data["tls.crt"]) > 0 && len(live.Data["tls.key"]) > 0, nil
}

// addKeys adds the keys the live Secret lacks, never touching one it holds,
// and returns them.
func addKeys(ctx context.Context, cs kubernetes.Interface, ns string, s generator.Secret) ([]string, error) {
	live, err := cs.CoreV1().Secrets(ns).Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading Secret %s: %w", s.Name, err)
	}
	data := map[string][]byte{}
	var added []string
	for k, v := range s.Data {
		if _, ok := live.Data[k]; !ok {
			data[k] = []byte(v)
			added = append(added, k)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	sort.Strings(added)
	patch, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return nil, err
	}
	if _, err := cs.CoreV1().Secrets(ns).Patch(ctx, s.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("adding keys to Secret %s: %w", s.Name, err)
	}
	return added, nil
}
