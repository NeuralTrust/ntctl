package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/prompt"
)

func withStatus(rel *release.Release, st rcommon.Status) *release.Release {
	rel.Info = &release.Info{Status: st}
	return rel
}

func TestInterruptedTellsHowToClearIt(t *testing.T) {
	cases := map[rcommon.Status]string{
		rcommon.StatusPendingInstall:  "`ntctl uninstall`",
		rcommon.StatusPendingUpgrade:  "`ntctl rollback`",
		rcommon.StatusPendingRollback: "`ntctl rollback`",
		rcommon.StatusDeployed:        "",
		rcommon.StatusFailed:          "",
	}
	for st, want := range cases {
		err := interrupted(withStatus(installed(gateOnly()), st), "neuraltrust")
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: err = %v, want none", st, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err = %v, want it to mention %s", st, err, want)
		}
	}
	if err := interrupted(nil, "neuraltrust"); err != nil {
		t.Errorf("no release: %v", err)
	}
}

// TestAddProductsOnAReleaseThatIsNotRunning: re-running install on a failed
// or interrupted release must not report "nothing to do", which left the
// operator with a broken install and no next step.
func TestAddProductsOnAReleaseThatIsNotRunning(t *testing.T) {
	cases := map[rcommon.Status]string{
		rcommon.StatusFailed:         "revision 1 failed, so nothing is added to it: run `ntctl doctor`",
		rcommon.StatusPendingInstall: "an install was interrupted",
	}
	for st, want := range cases {
		var out bytes.Buffer
		o := opts{products: "trustgate", release: "neuraltrust-platform"}
		o.kube.Namespace = "neuraltrust"
		in := prompt.New(context.Background(), strings.NewReader(""), &out, false)
		err := o.addProducts(context.Background(), in, &out, installFlags{}, nil, withStatus(installed(gateOnly()), st))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", st, err, want)
		}
		if strings.Contains(out.String(), "nothing to do") {
			t.Errorf("%s: reported nothing to do:\n%s", st, out.String())
		}
	}
}
