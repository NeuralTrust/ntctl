// Package helm wraps the Helm SDK actions ntctl uses, configured the way the
// helm CLI configures them so a release is the same whichever tool made it.
package helm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartapi "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/kube"
	ri "helm.sh/helm/v4/pkg/release"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"

	"github.com/NeuralTrust/ntctl/internal/chart"
)

// ErrNotReady means the release was applied but did not become ready in time.
var ErrNotReady = errors.New("workloads did not become ready in time")

// Logger is the handler Helm logs through: errors only, unless NTCTL_DEBUG is
// set. Helm's own warnings repeat what ntctl reports in plain words.
func Logger() slog.Handler {
	level := slog.LevelError
	if os.Getenv("NTCTL_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
}

// NewConfiguration returns an action configuration that logs through Logger.
func NewConfiguration() *action.Configuration {
	return action.NewConfiguration(action.ConfigurationSetLogger(Logger()))
}

// DefaultTimeout bounds install and upgrade, including the wait for readiness.
const DefaultTimeout = 15 * time.Minute

// RenderOptions mirror the flags of `helm template` that ntctl exposes.
type RenderOptions struct {
	Release     string
	Namespace   string
	KubeVersion string
	APIVersions []string
	// IsUpgrade renders as an upgrade of an existing release would.
	IsUpgrade bool
}

// Render returns the manifests `helm template` prints for the same inputs:
// the release manifest, then each hook with its source comment.
func Render(ctx context.Context, ch *chart.Loaded, vals map[string]any, o RenderOptions) (string, error) {
	inst := action.NewInstall(NewConfiguration())
	inst.DryRunStrategy = action.DryRunClient
	inst.ReleaseName = o.Release
	inst.Namespace = o.Namespace
	inst.Replace = true
	inst.APIVersions = common.VersionSet(o.APIVersions)
	inst.IsUpgrade = o.IsUpgrade
	if o.KubeVersion != "" {
		kv, err := common.ParseKubeVersion(o.KubeVersion)
		if err != nil {
			return "", fmt.Errorf("invalid kube version %q: %w", o.KubeVersion, err)
		}
		inst.KubeVersion = kv
	}
	r, err := inst.RunWithContext(ctx, ch.Chart, vals)
	if err != nil {
		return "", err
	}
	rel, err := v1(r)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintln(&b, strings.TrimSpace(rel.Manifest))
	for _, h := range rel.Hooks {
		fmt.Fprintf(&b, "---\n# Source: %s\n%s\n", h.Path, h.Manifest)
	}
	return b.String(), nil
}

// ErrStopped means the wait for readiness was stopped early: the release was
// applied and marked failed, as after a timeout.
var ErrStopped = errors.New("the wait for readiness was stopped")

// Install installs a new release and waits for it to become ready. On failure
// the resources are kept so the operator can see what went wrong. Cancelling
// stop (optional) ends the wait early; the release is then marked failed,
// exactly as when the timeout passes, and never left pending.
func Install(ctx context.Context, cfg *action.Configuration, ch *chart.Loaded, vals map[string]any, name, namespace string, timeout time.Duration, stop context.Context) (*release.Release, error) {
	inst := action.NewInstall(cfg)
	inst.ReleaseName = name
	inst.Namespace = namespace
	inst.ServerSideApply = true
	inst.WaitStrategy = kube.StatusWatcherStrategy
	inst.Timeout = timeout
	if stop != nil {
		inst.WaitOptions = []kube.WaitOption{kube.WithWaitMethodContext(stop)}
	}
	r, err := inst.RunWithContext(ctx, ch.Chart, vals)
	if err != nil && stop != nil && stop.Err() != nil && ctx.Err() == nil {
		rel, _ := v1(r)
		return rel, ErrStopped
	}
	rel, cerr := v1(r)
	if err != nil {
		return rel, readiness(err)
	}
	return rel, cerr
}

// readiness replaces Helm's per-resource wait report, which repeats what the
// status view shows, with ErrNotReady.
func readiness(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
		return ErrNotReady
	}
	return err
}

// UpgradeResult says what happened to a release whose upgrade failed.
type UpgradeResult struct {
	// RolledBackTo is the revision Helm restored, or 0 when there was no
	// earlier working revision and the failed one was left in place.
	RolledBackTo int
	// RollbackReady reports whether the restored revision became ready.
	RollbackReady bool
}

// Upgrade upgrades an existing release, keeping the values it was installed
// with and layering vals on top. The new chart's defaults apply: only values
// the operator set are carried over, so new image tags and defaults land. If
// it does not become ready, Helm rolls back to the last working revision when
// there is one.
func Upgrade(ctx context.Context, cfg *action.Configuration, ch *chart.Loaded, vals map[string]any, name, namespace string, timeout time.Duration) (UpgradeResult, error) {
	before, err := history(cfg, name)
	if err != nil {
		return UpgradeResult{}, err
	}
	last := 0
	if n := len(before); n > 0 {
		last = before[n-1].Version
	}
	working := LastWorking(before)
	up := action.NewUpgrade(cfg)
	up.Namespace = namespace
	// Not ReuseValues: Helm then renders the new chart with the old chart's
	// defaults, so an upgrade never picks up a new image tag.
	up.ResetThenReuseValues = true
	// Follow whatever apply method the release was made with, as `helm
	// upgrade` does, so releases installed by Helm 3 keep client-side apply.
	up.ServerSideApply = "auto"
	up.WaitStrategy = kube.StatusWatcherStrategy
	up.RollbackOnFailure = true
	up.Timeout = timeout
	up.MaxHistory = 10
	if _, err = up.RunWithContext(ctx, name, ch.Chart, vals); err == nil {
		return UpgradeResult{}, nil
	}
	// The upgrade wrote revision last+1; a rollback, when Helm found a working
	// revision to return to, wrote one more. Revision numbers are compared,
	// not history lengths: MaxHistory prunes old revisions on every write.
	var res UpgradeResult
	if after, herr := history(cfg, name); herr == nil && len(after) > 0 && working != nil {
		if newest := after[len(after)-1]; newest.Version > last+1 {
			res.RolledBackTo = working.Version
			res.RollbackReady = newest.Info.Status == rcommon.StatusDeployed
		}
	}
	return res, readiness(err)
}

// LastWorking returns the newest revision in hist that was deployed
// successfully, or nil when none ever was.
func LastWorking(hist []*release.Release) *release.Release {
	for i := len(hist) - 1; i >= 0; i-- {
		if st := hist[i].Info.Status; st == rcommon.StatusDeployed || st == rcommon.StatusSuperseded {
			return hist[i]
		}
	}
	return nil
}

// Plan returns the manifest an upgrade to ch with vals would apply, without
// applying anything. The cluster renders it in a server-side dry run, so
// templates that look up live objects (generated Secrets that are reused, for
// one) render as the real upgrade would. With reuse, vals are layered on the
// release's values over the new chart's defaults, as `ntctl upgrade` does;
// without, vals replace them.
func Plan(ctx context.Context, cfg *action.Configuration, ch chartapi.Charter, vals map[string]any, name, namespace string, reuse bool) (string, error) {
	up := action.NewUpgrade(cfg)
	up.Namespace = namespace
	up.DryRunStrategy = action.DryRunServer
	up.ResetThenReuseValues = reuse
	up.ResetValues = !reuse
	up.ServerSideApply = "auto"
	r, err := up.RunWithContext(ctx, name, ch, vals)
	if err != nil {
		return "", err
	}
	rel, err := v1(r)
	if err != nil {
		return "", err
	}
	return rel.Manifest, nil
}

// Rollback restores revision to of a release and waits, up to timeout, for
// it to become ready. It applies the way the release was made, as `helm
// rollback` does.
func Rollback(cfg *action.Configuration, name string, to int, timeout time.Duration) error {
	rb := action.NewRollback(cfg)
	rb.Version = to
	rb.WaitStrategy = kube.StatusWatcherStrategy
	rb.Timeout = timeout
	rb.ServerSideApply = "auto"
	rb.MaxHistory = 10
	if err := rb.Run(name); err != nil {
		return readiness(err)
	}
	return nil
}

// Uninstall removes a release and every resource it created, waiting up to
// timeout for them to be gone, as `helm uninstall --wait` does. Its history
// goes too, so the name can be installed again.
func Uninstall(cfg *action.Configuration, name string, timeout time.Duration) error {
	u := action.NewUninstall(cfg)
	u.WaitStrategy = kube.StatusWatcherStrategy
	u.DeletionPropagation = "background"
	u.Timeout = timeout
	if _, err := u.Run(name); err != nil {
		return readiness(err)
	}
	return nil
}

// History returns the release's revisions, oldest first.
func History(cfg *action.Configuration, name string) ([]*release.Release, error) {
	return history(cfg, name)
}

// history returns the release's revisions, oldest first.
func history(cfg *action.Configuration, name string) ([]*release.Release, error) {
	hist, err := action.NewHistory(cfg).Run(name)
	if err != nil {
		return nil, fmt.Errorf("reading history of %s: %w", name, err)
	}
	out := make([]*release.Release, 0, len(hist))
	for _, h := range hist {
		r, err := v1(h)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Current returns the release's newest revision, whatever its status (a
// failed or pending one included), or nil when there is no release.
func Current(cfg *action.Configuration, name string) (*release.Release, error) {
	r, err := action.NewStatus(cfg).Run(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading release %s: %w", name, err)
	}
	return v1(r)
}

func v1(r ri.Releaser) (*release.Release, error) {
	switch rel := r.(type) {
	case nil:
		return nil, nil
	case *release.Release:
		return rel, nil
	case release.Release:
		return &rel, nil
	default:
		return nil, fmt.Errorf("unsupported release type %T", r)
	}
}
