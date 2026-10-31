package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"helm.sh/helm/v4/pkg/action"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/auth"
	"github.com/NeuralTrust/ntctl/internal/changelog"
	"github.com/NeuralTrust/ntctl/internal/chart"
	"github.com/NeuralTrust/ntctl/internal/diff"
	"github.com/NeuralTrust/ntctl/internal/generator"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/manifest"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/redact"
	"github.com/NeuralTrust/ntctl/internal/report"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// changeFlags are shared by upgrade and rollback.
type changeFlags struct {
	yes           bool
	dryRun        bool
	showDiff      bool
	allowBreaking bool
	timeout       time.Duration
	// fullNotes is upgrade's --release-notes: every release's notes whole.
	fullNotes bool
	// secretsGenerator is upgrade's --secrets-generator: move an install
	// whose chart renders its credentials onto the secrets generator.
	secretsGenerator bool
}

func (f *changeFlags) register(fs interface {
	BoolVar(*bool, string, bool, string)
	DurationVar(*time.Duration, string, time.Duration, string)
}, what string) {
	fs.BoolVar(&f.yes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&f.dryRun, "dry-run", false, "show what would change, then stop")
	fs.BoolVar(&f.showDiff, "show-diff", false, "print the full diff of every changed resource (Secret values are never shown)")
	fs.BoolVar(&f.allowBreaking, "allow-breaking", false, "apply even changes Kubernetes cannot make in place, such as a new selector or a removed Secret or volume")
	fs.DurationVar(&f.timeout, "timeout", helm.DefaultTimeout, "how long to wait for the "+what+" to become ready")
}

// review prints what changes and decides whether to go on. It returns
// errPreflight when blocking changes are not allowed, and done when nothing
// more should happen.
func (f *changeFlags) review(out io.Writer, res diff.Result) (done bool, err error) {
	fmt.Fprintln(out, "\nWhat changes:")
	res.Print(out, f.showDiff)
	if len(res.Blocking()) > 0 && !f.allowBreaking {
		fmt.Fprintln(out, "\nStopped: nothing was changed. Adjust your values so these resources keep their current form,\n"+
			"or pass --allow-breaking to accept deleting and recreating them. Data in removed Secrets and volumes is lost.")
		return true, errPreflight
	}
	if f.dryRun {
		fmt.Fprintln(out, "\nDry run: nothing was changed.")
		return true, nil
	}
	return false, nil
}

const upgradeSummary = `Upgrade an existing install to a new chart version, keeping the values it
was installed with over the new chart's defaults, so new image tags apply.
-f files and flags are layered on top.

Before changing anything it shows the chart's release notes between the two
versions (--release-notes for every release in full), the image versions that
change, and every resource that changes, rendered by the cluster in a dry
run. Changes Kubernetes cannot apply in place (a new selector, a removed
Secret or volume) stop the upgrade unless --allow-breaking is given; a
removed resource the chart marks helm.sh/resource-policy: keep stays in the
cluster and does not stop it. If the
upgrade does not become ready, it is rolled back.

An install whose chart still renders its generated credentials is told when
the target chart can keep them in platform-secrets instead, created once by
the chart's secrets generator with their current values, which GitOps tools
never re-render. --secrets-generator makes that move; ntctl never makes it
unasked.

--products turns products on and leaves the others as they are, so naming one
never removes another. To add one on the installed chart:
  ntctl upgrade --products trustguard --version <installed version>
Its token Secrets are created as install does: from NT_TRUSTGUARD_* (or
NT_TRUSTGATE_*), from the console when signed in, or asked for with input
hidden. Secrets already in place are kept. --remove-products turns products
off; their workloads are deleted.

Operational telemetry (TrustGate and TrustGuard traces and metrics: in hybrid
installs sent to NeuralTrust, to help support diagnose your gateway; otherwise
to your in-cluster collector) keeps the install's setting unless
--operational-telemetry is given. To switch it while troubleshooting, change
nothing else and stay on the installed chart:
  ntctl upgrade --operational-telemetry=true --version <installed version>
  ntctl upgrade --operational-telemetry=false --version <installed version>
When the install never recorded a choice, ntctl asks once on a terminal, with
No as the default; with --yes or without a terminal it stays off.`

func runUpgrade(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	var o opts
	var f changeFlags
	var remove, org, gate, guard string
	fs := newFlags("upgrade", upgradeSummary, out, &o, true)
	f.register(fs, "upgrade")
	fs.BoolVar(&f.fullNotes, "release-notes", false, "print every release's notes in full, with its commits, instead of only the releases that have notes")
	fs.BoolVar(&f.secretsGenerator, "secrets-generator", false, "move the install's generated credentials into platform-secrets, which the chart's secrets generator creates once, keeping their values")
	fs.StringVar(&remove, "remove-products", "", "comma-separated products to turn off; their workloads are deleted")
	fs.StringVar(&org, "org", "", "signed in: organization ID for the tokens of an added product (default: the selected one)")
	fs.StringVar(&gate, "trustgate-instance", "", "signed in: ID or name of the private TrustGate to add (default: create one)")
	fs.StringVar(&guard, "trustguard-instance", "", "signed in: ID or name of the private TrustGuard to add (default: create one)")
	if err := parse(fs, args); err != nil {
		return err
	}
	in := prompt.New(ctx, stdin, out, !f.yes)
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return err
	}
	rel, err := helm.Current(cfg, o.release)
	if err != nil {
		return err
	}
	if err := interrupted(rel, o.kube.Namespace); err != nil {
		return err
	}
	// Without a release, applyChange says so.
	plan := &changePlan{consoles: &consolePlan{}}
	if rel != nil {
		var sess *session
		// Only an added product needs the console.
		if o.products != "" {
			if sess, err = openSession(); errors.Is(err, auth.ErrNotLoggedIn) {
				sess = nil
			} else if err != nil {
				return err
			}
		}
		pick := map[api.Product]string{api.TrustGate: gate, api.TrustGuard: guard}
		if plan, err = o.planChange(ctx, in, out, rel, remove, sess, org, pick, false, f.dryRun); err != nil {
			return err
		}
	}
	if f.secretsGenerator {
		o.extra = values.Merge(orEmpty(o.extra), map[string]any{"global": map[string]any{"secrets": map[string]any{"generator": map[string]any{"enabled": true}}}})
	}
	_, err = o.applyChange(ctx, in, out, f, plan)
	return err
}

// changePlan is an upgrade's product change, settled before the cluster is
// checked: what turns on and off, the token Secrets the added products need,
// and the console instances those tokens come from.
type changePlan struct {
	change   productChange
	create   []*corev1.Secret
	need     map[api.Product][]secrets.Spec
	sess     *session
	consoles *consolePlan
	// replace overwrites token Secrets that already exist (install's
	// --replace-secrets).
	replace bool
}

// planChange settles the product change of an upgrade of rel. Products in
// --products are turned on and those in remove off; the others keep their
// state. Operational telemetry is settled as upgradeTelemetry says. The added
// products' tokens are asked for now, so the checks see their Secrets; a dry
// run asks for none.
func (o *opts) planChange(ctx context.Context, in *prompt.Prompter, out io.Writer, rel *release.Release, remove string, sess *session, org string, pick map[api.Product]string, replace, dryRun bool) (*changePlan, error) {
	if err := o.sameMode(rel.Config); err != nil {
		return nil, err
	}
	if err := o.upgradeTelemetry(in, out, rel.Config); err != nil {
		return nil, err
	}
	files, err := values.Files(o.kube.Settings(), o.valueFiles)
	if err != nil {
		return nil, err
	}
	base := reuse(rel.Config, files)
	c := &changePlan{sess: sess, consoles: &consolePlan{}, replace: replace}
	if c.change, err = o.changeProducts(base, remove); err != nil {
		return nil, err
	}
	// Settled: the change goes in as values that leave the other products
	// alone, instead of --products naming the whole set.
	o.products = ""
	if !c.change.empty() {
		o.extra = values.ProductDelta(base, c.change.add, c.change.remove)
	}
	c.change.print(out)
	// The chart sends a hybrid install's telemetry through one DataAgent,
	// TrustGate's when it runs: adding it changes TrustGuard's too.
	if values.DeploymentMode(base) == "hybrid" && has(c.change.add, "trustgate") && has(values.Installed(base), "trustguard") {
		fmt.Fprintln(out, "TrustGate's DataAgent takes over sending this install's telemetry from TrustGuard's, so TrustGuard's DataAgent changes too; the list of changes below shows it.")
	}
	if len(c.change.add) == 0 {
		return c, nil
	}
	cs, err := o.kube.Clientset()
	if err != nil {
		return nil, err
	}
	if c.create, c.need, err = tokenSecrets(ctx, in, out, cs, o.kube.Namespace, sess, reuse(base, o.extra), c.change.add, replace, dryRun); err != nil {
		return nil, err
	}
	if c.consoles, err = planConsole(ctx, in, out, sess, org, c.need, pick, instanceName(base, o.release)); err != nil {
		return nil, err
	}
	// The release keeps the control plane it dials; tokens from a console
	// of another one would never be accepted.
	target := reuse(base, o.extra)
	if o.controlPlane != "" {
		target = values.Merge(target, values.ControlPlaneValues(o.controlPlane))
	}
	if _, err := pointAtConsole(target, c.consoles.controlPlane, false); err != nil {
		return nil, err
	}
	return c, nil
}

// applyChange checks the cluster, shows what the upgrade changes and, once
// confirmed, creates the added products' Secrets and upgrades, rolling back
// when it does not become ready. It returns the release's values after the
// upgrade, or nil when nothing was applied (a dry run, or nothing to change).
func (o *opts) applyChange(ctx context.Context, in *prompt.Prompter, out io.Writer, f changeFlags, c *changePlan) (map[string]any, error) {
	fmt.Fprintln(out, "Checking the cluster…")
	p, err := o.prepare(ctx, preflight.ForUpgrade, plannedKeys(c.create, c.need))
	if err != nil {
		return nil, err
	}
	if err := preflight.Print(out, p.results, false); err != nil {
		return nil, err
	}
	if preflight.Failed(p.results) {
		return nil, errPreflight
	}
	from, to := p.current.Chart.Metadata.Version, p.chart.Version
	fmt.Fprintf(out, "\nUpgrade %q in namespace %q from chart %s to %s\non %s.\n", o.release, o.kube.Namespace, from, to, o.kube.Target())
	if line := connects(reuse(p.current.Config, p.values), signedInConsole()); line != "" {
		fmt.Fprintln(out, line)
	}
	if o.chartPath == "" && from != to {
		releaseNotes(ctx, out, from, to, f.fullNotes)
	}

	if p.planned == nil {
		pl, err := helm.PlanWithHooks(ctx, p.cfg, p.chart.Chart, p.values, o.release, o.kube.Namespace, true)
		if err != nil {
			return nil, fmt.Errorf("the cluster rejected a dry run of the upgrade: %w", err)
		}
		p.planned = &pl
	}
	res, err := o.generatorChange(ctx, out, p, f.secretsGenerator)
	if err != nil {
		return nil, err
	}
	if done, err := f.review(out, res); done || err != nil {
		return nil, err
	}
	// A failed release renders the same as it was stored, yet it is not
	// running: applying it again is the retry once the cause is fixed.
	retry := failed(p.current)
	// A product change is applied even when it renders nothing new: the
	// release must record it, and its tokens be issued.
	if res.Empty() && from == to && !retry && c.change.empty() {
		fmt.Fprintln(out, "\nAlready up to date; nothing to upgrade.")
		return nil, nil
	}
	if retry {
		fmt.Fprintf(out, "\nRevision %d failed; this applies it again so it can become ready.\n", p.current.Version)
	}
	if !c.change.empty() {
		fmt.Fprintln(out)
	}
	if len(c.change.add) > 0 {
		fmt.Fprintf(out, "  Adds:     %s\n", strings.Join(c.change.add, ", "))
	}
	if len(c.change.remove) > 0 {
		fmt.Fprintf(out, "  Removes:  %s\n", strings.Join(c.change.remove, ", "))
	}
	c.consoles.describe(out)
	if !f.yes {
		if err := in.Confirm("Continue?"); err != nil {
			return nil, err
		}
	}
	issued, err := c.consoles.issue(ctx, out, c.sess, o.kube.Namespace)
	if err != nil {
		return nil, err
	}
	if d, err := c.consoles.issuedControlPlane(); err != nil {
		return nil, err
	} else if _, err := pointAtConsole(reuse(p.current.Config, p.values), d, false); err != nil {
		return nil, err
	}
	done, err := secrets.Apply(ctx, p.cs, append(c.create, issued...), c.replace)
	for _, d := range done {
		fmt.Fprintf(out, "Secret: %s\n", d)
	}
	if err != nil {
		return nil, err
	}
	hist, err := helm.History(p.cfg, o.release)
	if err != nil {
		return nil, err
	}
	if w := helm.LastWorking(hist); w != nil {
		fmt.Fprintf(out, "Upgrading… waiting up to %s; if it does not become ready, revision %d is restored.\n", f.timeout, w.Version)
	} else {
		fmt.Fprintf(out, "Upgrading… waiting up to %s. No revision has worked yet, so if this one does not become ready it is left in place for `ntctl doctor`.\n", f.timeout)
	}
	// Progress only: stopping an upgrade's wait would also cut short the
	// rollback Helm runs when it fails.
	w := waitWatch{cs: p.cs, cluster: p.cluster, ns: o.kube.Namespace, release: o.release, out: out, every: watchEvery}.start(ctx)
	result, upErr := helm.Upgrade(ctx, p.cfg, p.chart, p.values, o.release, o.kube.Namespace, f.timeout)
	w.finish()
	if st, err := report.Collect(ctx, p.cs, p.dyn, o.kube.Namespace); err == nil {
		fmt.Fprintln(out)
		st.Print(out)
	}
	if upErr != nil {
		o.printFailures(ctx, out)
	}
	switch {
	case upErr == nil:
	case result.RolledBackTo > 0 && result.RollbackReady:
		return nil, fmt.Errorf("upgrade failed (%w) and was rolled back to revision %d, which is running again", upErr, result.RolledBackTo)
	case result.RolledBackTo > 0:
		return nil, fmt.Errorf("upgrade failed (%w); it was rolled back to revision %d, but that is not ready either. Run `ntctl doctor`", upErr, result.RolledBackTo)
	default:
		return nil, fmt.Errorf("upgrade failed (%w); there was no earlier working revision to roll back to, so it was left in place: %s", upErr, fixThenUpgrade)
	}
	vals := reuse(p.current.Config, p.values)
	c.consoles.finalize(ctx, out, c.sess, vals)
	fmt.Fprintln(out, "\nUpgraded.")
	return vals, nil
}

// generatorChange compares the deployed manifest with the planned one, and
// settles the secrets generator. An install whose chart renders its own
// credentials is told, in one line, that the target chart can move them onto
// the generator; ntctl never does so unasked. With --secrets-generator, a
// target chart that does not run the generator for this install is an error,
// not a silent no-op.
func (o *opts) generatorChange(ctx context.Context, out io.Writer, p *prepared, enable bool) (diff.Result, error) {
	res, err := compare(p.current.Manifest, p.planned.Manifest)
	if err != nil {
		return res, err
	}
	planned := generator.Target(p.objects)
	var deployedHooks []manifest.Object
	for _, h := range p.current.Hooks {
		if objs, err := manifest.Parse(h.Manifest); err == nil {
			deployedHooks = append(deployedHooks, objs...)
		}
	}
	optedOut := values.Get(reuse(p.current.Config, p.values), "global", "secrets", "generator", "enabled") == false
	switch {
	case enable && planned == "":
		return res, fmt.Errorf("chart %s does not run the secrets generator for this install, so --secrets-generator changes nothing; upgrade to a newer chart, or leave the flag out", p.chart.Version)
	case generator.Target(deployedHooks) != "":
	case enable:
		fmt.Fprintf(out, "\nThe secrets generator creates %s before the upgrade, with the values the install uses now; the Secrets the chart rendered\nthem in stay in the cluster, no longer read.\n", planned)
	case planned == "" && !optedOut && o.generatorAvailable(ctx, p):
		fmt.Fprintf(out, "\nThis install's chart renders its generated credentials on every upgrade. Chart %s can keep them instead in\n"+
			"platform-secrets, created once by its secrets generator with their current values, which GitOps tools never re-render:\n"+
			"add --secrets-generator to move them.\n", p.chart.Version)
	}
	return res, nil
}

// generatorAvailable reports whether the target chart runs the secrets
// generator for this install once it is turned on, from a client render: the
// chart, not ntctl, knows which installs it covers.
func (o *opts) generatorAvailable(ctx context.Context, p *prepared) bool {
	on := values.Merge(reuse(p.current.Config, p.values), map[string]any{"global": map[string]any{"secrets": map[string]any{"generator": map[string]any{"enabled": true}}}})
	rendered, err := helm.Render(ctx, p.chart, on, helm.RenderOptions{Release: o.release, Namespace: o.kube.Namespace, KubeVersion: p.cluster.Version, IsUpgrade: true})
	if err != nil {
		return false
	}
	objs, err := manifest.Parse(rendered)
	return err == nil && generator.Target(objs) != ""
}

// releaseNotes prints the chart's notes between two versions. They help but
// are not required, so a failure to fetch them is only reported.
func releaseNotes(ctx context.Context, out io.Writer, from, to string, full bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	entries, err := changelog.Notes(ctx, &http.Client{Timeout: 15 * time.Second}, from, to)
	if err != nil {
		fmt.Fprintf(out, "\nRelease notes are unavailable (%v).\n", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	what := "releases"
	if len(entries) == 1 {
		what = "release"
	}
	fmt.Fprintf(out, "\nRelease notes, %s to %s (%d %s):\n", from, to, len(entries), what)
	changelog.Print(out, entries, full)
	if !full {
		fmt.Fprintf(out, "  Every release in full: --release-notes, or %s\n", changelog.NotesURL)
	}
}

func compare(current, target string) (diff.Result, error) {
	a, err := manifest.Parse(current)
	if err != nil {
		return diff.Result{}, fmt.Errorf("reading the deployed manifest: %w", err)
	}
	b, err := manifest.Parse(target)
	if err != nil {
		return diff.Result{}, fmt.Errorf("reading the target manifest: %w", err)
	}
	return diff.Compare(a, b), nil
}

func runHistory(_ context.Context, args []string, out io.Writer) error {
	var o opts
	fs := newFlags("history", "Show the revisions of an install, oldest first.", out, &o, false)
	fs.StringVar(&o.output, "output", "text", "text or json")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return err
	}
	hist, err := helm.History(cfg, o.release)
	if err != nil {
		return err
	}
	if o.output == "json" {
		var rows []map[string]any
		for _, r := range hist {
			rows = append(rows, map[string]any{
				"revision": r.Version, "updated": r.Info.LastDeployed, "status": r.Info.Status.String(),
				"chart": r.Chart.Metadata.Version, "appVersion": r.Chart.Metadata.AppVersion, "description": r.Info.Description,
			})
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REVISION\tUPDATED\tSTATUS\tCHART\tDESCRIPTION")
	for _, r := range hist {
		desc, _, _ := strings.Cut(r.Info.Description, "\n")
		if len(desc) > 80 {
			desc = desc[:80] + "…"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", r.Version, r.Info.LastDeployed.Format("2006-01-02 15:04"), r.Info.Status, r.Chart.Metadata.Version, desc)
	}
	return tw.Flush()
}

const rollbackSummary = `Roll an install back to an earlier revision: by default the newest one that
worked, or --to N (see "ntctl history"). It shows what changes and asks before
applying, and stops on changes Kubernetes cannot make in place unless
--allow-breaking is given.

A rollback re-applies what the revision ran and runs none of the chart's
install or upgrade hooks, the secrets generator among them. It therefore also
stops when that revision reads a Secret no longer in the cluster, such as a
generator Secret deleted as a leftover, until it is restored or
--allow-breaking is given.`

func runRollback(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	var o opts
	var f changeFlags
	var to int
	fs := newFlags("rollback", rollbackSummary, out, &o, false)
	fs.IntVar(&to, "to", 0, "revision to return to (default: the newest earlier one that worked)")
	f.register(fs, "rollback")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return err
	}
	hist, err := helm.History(cfg, o.release)
	if err != nil {
		return err
	}
	if len(hist) == 0 {
		return fmt.Errorf("no release %q in namespace %q", o.release, o.kube.Namespace)
	}
	cur := hist[len(hist)-1]
	target := rollbackTarget(hist, to)
	switch {
	case target == nil && to > 0:
		return fmt.Errorf("release %q has no revision %d; see `ntctl history`", o.release, to)
	case target == nil:
		return fmt.Errorf("release %q has no earlier revision that worked, so there is nothing to roll back to; %s", o.release, fixThenUpgrade)
	case target.Version == cur.Version:
		return fmt.Errorf("revision %d is the current one", to)
	}
	fmt.Fprintf(out, "Roll back %q in namespace %q from revision %d (chart %s, %s) to revision %d (chart %s)\non %s.\n",
		o.release, o.kube.Namespace, cur.Version, cur.Chart.Metadata.Version, cur.Info.Status, target.Version, target.Chart.Metadata.Version, o.kube.Target())
	res, err := compare(cur.Manifest, target.Manifest)
	if err != nil {
		return err
	}
	cs, err := o.kube.Clientset()
	if err != nil {
		return err
	}
	missing, err := unresolved(ctx, cs, o.kube.Namespace, target)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "\nRevision %d reads Secrets that are no longer in the cluster, and a rollback does not run the\n"+
			"chart's secrets generator to create them, so its pods would start without these credentials:\n", target.Version)
		for _, m := range missing {
			fmt.Fprintf(out, "  %s\n", m)
		}
		if !f.allowBreaking {
			fmt.Fprintln(out, "\nStopped: nothing was changed. Restore them from your backup first, or pass --allow-breaking to roll back anyway.")
			return errPreflight
		}
	}
	if done, err := f.review(out, res); done || err != nil {
		return err
	}
	if !f.yes {
		if err := prompt.New(ctx, stdin, out, true).Confirm("Continue?"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Rolling back… waiting up to %s.\n", f.timeout)
	rbErr := helm.Rollback(cfg, o.release, target.Version, f.timeout)
	if cs, dyn, err := o.clients(); err == nil {
		if st, err := report.Collect(ctx, cs, dyn, o.kube.Namespace); err == nil {
			fmt.Fprintln(out)
			st.Print(out)
		}
	}
	if rbErr != nil {
		return fmt.Errorf("rollback did not complete: %w. Run `ntctl doctor`", rbErr)
	}
	fmt.Fprintf(out, "\nRolled back: the new revision %d runs what revision %d ran.\n", cur.Version+1, target.Version)
	return nil
}

// unresolved lists the Secrets, and keys, the workloads of revision rel read
// that neither its manifest renders nor the cluster holds. A rollback applies
// the stored manifest and runs no pre-install or pre-upgrade hook, so nothing
// recreates them: a Secret the generator wrote and an operator deleted since
// as a leftover is the case this catches. Optional references count only for
// the Secret that revision's generator wrote, which the chart reads that way.
func unresolved(ctx context.Context, cs kubernetes.Interface, ns string, rel *release.Release) ([]string, error) {
	objs, err := manifest.Parse(rel.Manifest)
	if err != nil {
		return nil, fmt.Errorf("reading revision %d: %w", rel.Version, err)
	}
	var hooks []manifest.Object
	for _, h := range rel.Hooks {
		ho, err := manifest.Parse(h.Manifest)
		if err != nil {
			return nil, fmt.Errorf("reading revision %d's hooks: %w", rel.Version, err)
		}
		hooks = append(hooks, ho...)
	}
	gen := generator.Target(hooks)
	rendered := map[string]bool{}
	for _, o := range objs {
		if o.Kind() == "Secret" {
			rendered[o.Name()] = true
		}
	}
	live := map[string]*corev1.Secret{}
	gone := map[string]bool{}
	var out []string
	for _, r := range manifest.References(objs) {
		if rendered[r.Name] || (r.Optional && r.Name != gen) {
			continue
		}
		s, ok := live[r.Name]
		if !ok {
			got, err := cs.CoreV1().Secrets(ns).Get(ctx, r.Name, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return nil, fmt.Errorf("reading Secret %s: %w", r.Name, err)
			default:
				s = got
			}
			live[r.Name] = s
		}
		switch {
		case s == nil:
			if !gone[r.Name] {
				gone[r.Name] = true
				out = append(out, fmt.Sprintf("Secret %s (read by %s)", r.Name, r.By))
			}
		case r.Key != "" && !r.Optional:
			if _, ok := s.Data[r.Key]; !ok {
				out = append(out, fmt.Sprintf("key %s of Secret %s (read by %s)", r.Key, r.Name, r.By))
			}
		}
	}
	return out, nil
}

// rollbackTarget returns revision to, or when to is 0 the newest revision
// before the current one that was deployed successfully.
func rollbackTarget(hist []*release.Release, to int) *release.Release {
	if to > 0 {
		for _, r := range hist {
			if r.Version == to {
				return r
			}
		}
		return nil
	}
	for i := len(hist) - 2; i >= 0; i-- {
		if st := hist[i].Info.Status; st == rcommon.StatusDeployed || st == rcommon.StatusSuperseded {
			return hist[i]
		}
	}
	return nil
}

const adoptSummary = `Check that an install made with plain Helm can be managed with ntctl,
without changing it. ntctl re-renders the release's own chart with the values
it was installed with, in a dry run, and compares that with what is deployed.
When they match, it writes those values to a file with secrets removed, for
your records or GitOps. Nothing in the cluster changes, and ntctl adds none of
its own Secrets: later upgrades keep the release's values and Secret names.`

func runAdopt(ctx context.Context, args []string, out io.Writer) error {
	var o opts
	var file string
	var showDiff bool
	fs := newFlags("adopt", adoptSummary, out, &o, false)
	fs.StringVar(&file, "values-file", "neuraltrust-values.yaml", "where to write the release's values, secrets removed (empty to skip)")
	fs.BoolVar(&showDiff, "show-diff", false, "print the full diff when the release does not re-render identically")
	fs.StringVar(&o.chartPath, "chart", "", "local chart directory or .tgz of the release's chart version, for clusters without access to the chart registry")
	if err := parse(fs, args); err != nil {
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
	if rel == nil {
		return fmt.Errorf("no release %q in namespace %q; pass --namespace and --release", o.release, o.kube.Namespace)
	}
	fmt.Fprintf(out, "Release %s: chart %s, revision %d, %s, on %s.\n", rel.Name, rel.Chart.Metadata.Version, rel.Version, status(rel), o.kube.Target())
	if err := interrupted(rel, o.kube.Namespace); err != nil {
		return err
	}
	if failed(rel) {
		fmt.Fprintf(out, "Revision %d failed: adopt compares it with what it applied, but it is not running; %s.\n", rel.Version, fixThenUpgrade)
	}
	// The chart stored in the release has no subcharts (Helm does not keep
	// them), so the release's own version is loaded from its source.
	want := rel.Chart.Metadata.Version
	ch, err := chart.Load(o.kube.Settings(), chart.Source{Path: o.chartPath, Version: want})
	if err != nil {
		return fmt.Errorf("loading chart %s, the release's version: %w; without access to the chart registry, pass --chart with that version's directory or .tgz", want, err)
	}
	if ch.Version != want {
		return fmt.Errorf("--chart is version %s, but the release runs %s; pass the chart the release was installed with", ch.Version, want)
	}
	planned, err := helm.Plan(ctx, cfg, ch.Chart, rel.Config, o.release, o.kube.Namespace, false)
	if err != nil {
		return fmt.Errorf("the cluster rejected a dry run of the release's own chart: %w", err)
	}
	res, err := compare(rel.Manifest, planned)
	if err != nil {
		return err
	}
	if !res.Empty() {
		fmt.Fprintln(out, "\nThe release does not re-render as deployed:")
		res.Print(out, showDiff)
		fmt.Fprintln(out, "\nThis happens when the install used a post-renderer or kustomize step, or when templates read\n"+
			"live objects that changed since. ntctl would apply these differences on its next upgrade;\n"+
			"review them (--show-diff) before running `ntctl upgrade --dry-run`.")
		return errPreflight
	}
	fmt.Fprintln(out, "\nThe release re-renders exactly as deployed: ntctl can upgrade, roll back and diagnose it.")
	if file == "" {
		return nil
	}
	clean, _ := redact.Value(deepCopy(rel.Config)).(map[string]any)
	removed := redactedPaths(rel.Config, clean, "")
	if err := values.Save(file, clean); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote the release's values to %s.\n", file)
	if len(removed) > 0 {
		fmt.Fprintf(out, "%d values looked like secrets and were left out: %s\n"+
			"Keep them in your own values file, or better in a Secret the chart reads (existingSecret).\n",
			len(removed), strings.Join(removed, ", "))
	}
	return nil
}

// deepCopy copies values through JSON, so redacting the copy leaves every
// nested map and list of the original untouched.
func deepCopy(m map[string]any) map[string]any {
	out := map[string]any{}
	b, err := json.Marshal(m)
	if err == nil {
		_ = json.Unmarshal(b, &out) // a document that marshalled unmarshals
	}
	return out
}

// redactedPaths lists the dotted paths whose value redaction changed.
func redactedPaths(orig, clean any, prefix string) []string {
	var out []string
	switch o := orig.(type) {
	case map[string]any:
		c, _ := clean.(map[string]any)
		for k, v := range o {
			out = append(out, redactedPaths(v, c[k], join(prefix, k))...)
		}
	case []any:
		c, _ := clean.([]any)
		for i, v := range o {
			if i < len(c) {
				out = append(out, redactedPaths(v, c[i], fmt.Sprintf("%s[%d]", prefix, i))...)
			}
		}
	case string:
		if s, _ := clean.(string); s != o {
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return out
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}
