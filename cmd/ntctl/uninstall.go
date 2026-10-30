package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	release "helm.sh/helm/v4/pkg/release/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/generator"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/manifest"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/setup"
)

const uninstallSummary = `Remove an install: the release and every resource it created. It works on
a release that failed or whose install was interrupted.

Your data and credentials are kept unless you ask. Only --delete-data deletes
the volumes in the namespace (PostgreSQL, ClickHouse and so on) and the
Secrets that hold the install's generated credentials: those the chart's
secrets generator made (platform-secrets) and those the chart keeps on purpose.
They go together because the keys decrypt what the volumes hold. The Secrets
ntctl created (tokens, registry key, datastore and admin credentials) go only
with --delete-secrets. The namespace stays, and private TrustGate and
TrustGuard instances stay in the console.

It shows what it removes and asks you to type the namespace to confirm;
--yes skips that, for scripts.`

func runUninstall(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	var o opts
	var yes, deleteData, deleteSecrets bool
	var timeout time.Duration
	fs := newFlags("uninstall", uninstallSummary, out, &o, false)
	fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&deleteData, "delete-data", false, "also delete the namespace's volumes (PersistentVolumeClaims) and the Secrets of the install's generated credentials: their data is lost")
	fs.BoolVar(&deleteSecrets, "delete-secrets", false, "also delete the Secrets ntctl created: tokens, registry key, datastore and admin credentials")
	fs.DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for the resources to be gone")
	if err := parse(fs, args); err != nil {
		return err
	}
	ns := o.kube.Namespace
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return err
	}
	rel, err := helm.Current(cfg, o.release)
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("no release %q in namespace %q; pass --namespace and --release", o.release, ns)
	}
	cs, err := o.kube.Clientset()
	if err != nil {
		return err
	}
	volumes, err := volumeClaims(ctx, cs, ns)
	if err != nil {
		return err
	}
	own, err := ownSecrets(ctx, cs, ns)
	if err != nil {
		return err
	}
	kept, err := keptSecrets(ctx, cs, ns, rel, own)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Remove release %q (chart %s, revision %d, %s) from namespace %q\non %s.\n",
		o.release, rel.Chart.Metadata.Version, rel.Version, status(rel), ns, o.kube.Target())
	fmt.Fprintln(out, "  Deletes:  every resource the release manages")
	describe := func(what string, names []string, deleted bool, flag string) {
		if len(names) == 0 {
			return
		}
		if deleted {
			fmt.Fprintf(out, "  Deletes:  %s: %s\n", what, strings.Join(names, ", "))
			return
		}
		fmt.Fprintf(out, "  Keeps:    %s: %s (%s deletes them)\n", what, strings.Join(names, ", "), flag)
	}
	describe("volumes", volumes, deleteData, "--delete-data")
	describe("Secrets of the generated credentials", kept, deleteData, "--delete-data")
	describe("Secrets ntctl created", own, deleteSecrets, "--delete-secrets")
	fmt.Fprintf(out, "  Keeps:    namespace %s, and the private instances in the console\n", ns)
	if !yes {
		in := prompt.New(ctx, stdin, out, true)
		typed, err := in.Text(fmt.Sprintf("Type the namespace (%s) to confirm", ns), "")
		if err != nil {
			return err
		}
		if strings.TrimSpace(typed) != ns {
			return fmt.Errorf("%q is not %q; nothing was removed", typed, ns)
		}
	}

	fmt.Fprintf(out, "Uninstalling… waiting up to %s for the resources to be gone.\n", timeout)
	if err := helm.Uninstall(cfg, o.release, timeout); err != nil {
		return fmt.Errorf("uninstall did not complete: %w; run it again to finish", err)
	}
	fmt.Fprintf(out, "Removed release %q.\n", o.release)
	if deleteData {
		if err := deleteEach(ctx, out, "volume", volumes, func(ctx context.Context, name string) error {
			return cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}); err != nil {
			return err
		}
	}
	if deleteData {
		if err := deleteEach(ctx, out, "Secret", kept, func(ctx context.Context, name string) error {
			return cs.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}); err != nil {
			return err
		}
	}
	if deleteSecrets {
		if err := deleteEach(ctx, out, "Secret", own, func(ctx context.Context, name string) error {
			return cs.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}); err != nil {
			return err
		}
	}
	return nil
}

// volumeClaims lists the namespace's PersistentVolumeClaims. A StatefulSet's
// volumes are not part of its release, so uninstalling leaves them.
func volumeClaims(ctx context.Context, cs kubernetes.Interface, ns string) ([]string, error) {
	list, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	var out []string
	for _, c := range list.Items {
		out = append(out, c.Name)
	}
	return out, nil
}

// ownSecrets lists the Secrets ntctl creates that exist in the namespace.
func ownSecrets(ctx context.Context, cs kubernetes.Interface, ns string) ([]string, error) {
	var out []string
	for _, name := range setup.OwnSecrets() {
		_, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("reading Secret %s: %w", name, err)
		default:
			out = append(out, name)
		}
	}
	return out, nil
}

// keptSecrets lists the Secrets in the namespace that outlive the release
// and hold its generated credentials: those the secrets generator manages,
// those the release renders with helm.sh/resource-policy: keep, and those its
// hooks made, which Helm never tracks. ntctl's own Secrets are left out.
func keptSecrets(ctx context.Context, cs kubernetes.Interface, ns string, rel *release.Release, own []string) ([]string, error) {
	names, err := generator.Owned(ctx, cs, ns)
	if err != nil {
		return nil, err
	}
	candidates := map[string]bool{}
	docs := []string{rel.Manifest}
	for _, h := range rel.Hooks {
		docs = append(docs, h.Manifest)
	}
	for i, doc := range docs {
		objs, err := manifest.Parse(doc)
		if err != nil {
			return nil, fmt.Errorf("reading the release: %w", err)
		}
		for _, o := range objs {
			if o.Kind() != "Secret" {
				continue
			}
			meta, _ := o["metadata"].(map[string]any)
			ann, _ := meta["annotations"].(map[string]any)
			if i > 0 || ann["helm.sh/resource-policy"] == "keep" {
				candidates[o.Name()] = true
			}
		}
	}
	for name := range candidates {
		_, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("reading Secret %s: %w", name, err)
		default:
			names = append(names, name)
		}
	}
	var out []string
	for _, n := range names {
		if !slices.Contains(own, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func deleteEach(ctx context.Context, out io.Writer, kind string, names []string, del func(context.Context, string) error) error {
	for _, name := range names {
		if err := del(ctx, name); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %s %s: %w", kind, name, err)
		}
		fmt.Fprintf(out, "Deleted %s %s.\n", kind, name)
	}
	return nil
}
