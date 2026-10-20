package main

import (
	"fmt"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
)

// fixThenUpgrade is the way out of a release that did not become ready.
const fixThenUpgrade = "run `ntctl doctor` for the cause, fix it, then run `ntctl upgrade` to apply the release again"

// status is the release's status, or "" when it has none recorded.
func status(rel *release.Release) rcommon.Status {
	if rel == nil || rel.Info == nil {
		return ""
	}
	return rel.Info.Status
}

// failed reports whether the release's newest revision failed.
func failed(rel *release.Release) bool { return status(rel) == rcommon.StatusFailed }

// interrupted refuses a release whose install, upgrade or rollback never
// finished, usually because ntctl or helm was stopped mid-way. Helm keeps it
// locked in a pending state, and every later change fails until it is
// cleared, so the operator is told how.
func interrupted(rel *release.Release, ns string) error {
	st := status(rel)
	if !st.IsPending() {
		return nil
	}
	if st == rcommon.StatusPendingInstall {
		return fmt.Errorf("release %q in namespace %q is %s: an install was interrupted before it finished. "+
			"If no install is running now, remove it with `ntctl uninstall`, then install again", rel.Name, ns, st)
	}
	return fmt.Errorf("release %q in namespace %q is %s: a change was interrupted before it finished. "+
		"If none is running now, return to the last working revision with `ntctl rollback`, then upgrade again", rel.Name, ns, st)
}
