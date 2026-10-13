package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"helm.sh/helm/v4/pkg/action"
	release "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/bundle"
	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/doctor"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/version"
)

const doctorSummary = `Diagnose an install: the preflight checks against what is deployed, then
what goes wrong at runtime (images that do not pull, missing Secrets, crashing
or OOM-killed containers, pods that cannot be scheduled, volumes and Jobs that
never finish, endpoints without an address). Each finding has a code and a fix.
Changes nothing. Works on installs made with plain Helm too.`

// diagnosis is a doctor run and what it looked at.
type diagnosis struct {
	cs       kubernetes.Interface
	cfg      *action.Configuration
	cluster  *detect.Cluster
	current  *release.Release
	findings []preflight.Result
}

func (o *opts) diagnose(ctx context.Context, logLines int64) (*diagnosis, error) {
	cs, err := o.kube.Clientset()
	if err != nil {
		return nil, err
	}
	cluster, err := detect.Inspect(ctx, cs)
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
	findings, err := doctor.Run(ctx, doctor.Input{
		Clientset: cs, Namespace: o.kube.Namespace, Release: o.release, Current: current, Cluster: cluster, LogLines: logLines,
	})
	if err != nil {
		return nil, err
	}
	return &diagnosis{cs: cs, cfg: cfg, cluster: cluster, current: current, findings: findings}, nil
}

func runDoctor(ctx context.Context, args []string, out io.Writer) error {
	var o opts
	var logLines int64
	fs := newFlags("doctor", doctorSummary, out, &o, false)
	fs.StringVar(&o.output, "output", "text", "text or json")
	fs.Int64Var(&logLines, "log-lines", 10, "log lines to show for a crashing container")
	if err := parse(fs, args); err != nil {
		return err
	}
	d, err := o.diagnose(ctx, logLines)
	if err != nil {
		return err
	}
	if o.output != "json" {
		fmt.Fprintf(out, "Checking %s, namespace %q.\n\n", o.kube.Target(), o.kube.Namespace)
	}
	if err := preflight.Print(out, d.findings, o.output == "json"); err != nil {
		return err
	}
	if preflight.Failed(d.findings) {
		if o.output != "json" {
			fmt.Fprintln(out, "\nFix the FAIL findings above. `ntctl support-bundle` packages the evidence for NeuralTrust support.")
		}
		return errPreflight
	}
	if o.output != "json" {
		fmt.Fprintln(out, "\nNo problems found.")
	}
	return nil
}

const bundleSummary = `Write a support bundle: a .tar.gz with the doctor report, the release's
values and manifest, the namespace's resources, events and recent logs, node
capacity and versions. Secret values, tokens, passwords and connection strings
are removed; the bundle's README lists exactly what. Changes nothing in the
cluster. Look through the archive before sending it to NeuralTrust support.`

func runSupportBundle(ctx context.Context, args []string, out io.Writer) error {
	var o opts
	var file string
	var logLines int64
	fs := newFlags("support-bundle", bundleSummary, out, &o, false)
	fs.StringVar(&file, "output-file", "", "where to write the bundle (default: ntctl-support-<namespace>-<time>.tar.gz)")
	fs.Int64Var(&logLines, "log-lines", 500, "log lines to keep per container")
	if err := parse(fs, args); err != nil {
		return err
	}
	now := time.Now()
	if file == "" {
		file = fmt.Sprintf("ntctl-support-%s-%s.tar.gz", o.kube.Namespace, now.UTC().Format("20060102-150405"))
	}
	fmt.Fprintln(out, "Diagnosing…")
	d, err := o.diagnose(ctx, 10)
	if err != nil {
		return err
	}
	var hist []*release.Release
	if d.current != nil {
		if h, err := action.NewHistory(d.cfg).Run(o.release); err == nil {
			for _, r := range h {
				if rel, ok := r.(*release.Release); ok {
					hist = append(hist, rel)
				}
			}
		}
	}
	versions := map[string]string{
		"ntctl": version.Version, "kubernetes": d.cluster.Version, "platform": d.cluster.Platform,
		"namespace": o.kube.Namespace, "release": o.release,
	}
	if d.current != nil {
		versions["chart"] = d.current.Chart.Metadata.Version
		versions["releaseStatus"] = d.current.Info.Status.String()
	}

	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- path chosen by the operator
	if err != nil {
		return fmt.Errorf("creating %s: %w", file, err)
	}
	fmt.Fprintln(out, "Collecting resources, events and logs…")
	sum, err := bundle.Write(ctx, f, bundle.Input{
		Clientset: d.cs, Namespace: o.kube.Namespace, Release: o.release, Current: d.current, History: hist,
		Findings: d.findings, Versions: versions, LogLines: logLines, Now: now,
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(file) // a partial bundle is of no use
		return fmt.Errorf("writing the bundle: %w", err)
	}
	fmt.Fprintf(out, "\nWrote %s (%d files).\n", file, sum.Files)
	if len(sum.Errors) > 0 {
		fmt.Fprintf(out, "%d items could not be collected; they are listed in errors.txt inside the bundle.\n", len(sum.Errors))
	}
	fmt.Fprintf(out, "Secret values were removed; README.md inside lists what. Look through it before sending:\n  tar -tzf %s\n", file)
	return nil
}
