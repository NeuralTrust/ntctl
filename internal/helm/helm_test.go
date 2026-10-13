package helm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"

	"github.com/NeuralTrust/ntctl/internal/chart"
)

// fakeCluster is a Helm configuration over in-memory release storage and a
// kube client whose readiness wait fails when notReady is set.
type fakeCluster struct {
	cfg  *action.Configuration
	kube *kubefake.FailingKubeClient
}

func newFakeCluster() *fakeCluster {
	k := &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(slog.DiscardHandler))
	cfg.Releases = storage.Init(driver.NewMemory())
	cfg.KubeClient = k
	cfg.Capabilities = common.DefaultCapabilities
	return &fakeCluster{cfg: cfg, kube: k}
}

func (f *fakeCluster) notReady(on bool) {
	f.kube.WaitError = nil
	if on {
		f.kube.WaitError = context.DeadlineExceeded
	}
}

func tinyChart() *chart.Loaded {
	ch := &chartv2.Chart{
		Metadata: &chartv2.Metadata{APIVersion: "v2", Name: "tiny", Version: "1.0.0"},
		Templates: []*common.File{{Name: "templates/cm.yaml", Data: []byte(
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: tiny\ndata:\n  v: {{ .Values.v | default \"0\" | quote }}\n")}},
	}
	return &chart.Loaded{Chart: ch, Name: "tiny", Version: "1.0.0"}
}

func TestUpgradeRollsBackToTheLastWorkingRevision(t *testing.T) {
	ctx := context.Background()
	f := newFakeCluster()
	if _, err := Install(ctx, f.cfg, tinyChart(), map[string]any{"v": "1"}, "rel", "ns", time.Minute); err != nil {
		t.Fatalf("install: %v", err)
	}
	f.notReady(true)
	res, err := Upgrade(ctx, f.cfg, tinyChart(), map[string]any{"v": "2"}, "rel", "ns", time.Minute)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if res.RolledBackTo != 1 {
		t.Fatalf("RolledBackTo = %d, want 1", res.RolledBackTo)
	}
	// The fake cluster is never ready, so the restored revision is not either.
	if res.RollbackReady {
		t.Error("RollbackReady = true on a cluster that never becomes ready")
	}
	hist, _ := history(f.cfg, "rel")
	if last := hist[len(hist)-1]; last.Config["v"] != "1" {
		t.Fatalf("the rollback revision should carry revision 1's values, got %v", last.Config)
	}
}

func TestUpgradeWithNothingToRollBackTo(t *testing.T) {
	ctx := context.Background()
	f := newFakeCluster()
	f.notReady(true)
	if _, err := Install(ctx, f.cfg, tinyChart(), nil, "rel", "ns", time.Minute); !errors.Is(err, ErrNotReady) {
		t.Fatalf("install err = %v, want ErrNotReady", err)
	}
	res, err := Upgrade(ctx, f.cfg, tinyChart(), nil, "rel", "ns", time.Minute)
	if err == nil {
		t.Fatal("upgrade of a never-ready release should fail while it stays unready")
	}
	if res.RolledBackTo != 0 {
		t.Fatalf("RolledBackTo = %d, want 0: there was no working revision", res.RolledBackTo)
	}
}

func TestUpgradeKeepsValues(t *testing.T) {
	ctx := context.Background()
	f := newFakeCluster()
	if _, err := Install(ctx, f.cfg, tinyChart(), map[string]any{"v": "1", "keep": "me"}, "rel", "ns", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := Upgrade(ctx, f.cfg, tinyChart(), map[string]any{"v": "2"}, "rel", "ns", time.Minute); err != nil {
		t.Fatal(err)
	}
	cur, _ := Current(f.cfg, "rel")
	if cur.Config["keep"] != "me" || cur.Config["v"] != "2" {
		t.Fatalf("values after upgrade = %v, want keep=me and v=2", cur.Config)
	}
}

func TestRollbackRestoresAnEarlierRevision(t *testing.T) {
	ctx := context.Background()
	f := newFakeCluster()
	if _, err := Install(ctx, f.cfg, tinyChart(), map[string]any{"v": "1"}, "rel", "ns", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := Upgrade(ctx, f.cfg, tinyChart(), map[string]any{"v": "2"}, "rel", "ns", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(f.cfg, "rel", 1, time.Minute); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	cur, _ := Current(f.cfg, "rel")
	if cur.Version != 3 || cur.Config["v"] != "1" {
		t.Fatalf("after rollback: revision %d values %v, want revision 3 with revision 1's values", cur.Version, cur.Config)
	}
	hist, err := History(f.cfg, "rel")
	if err != nil || len(hist) != 3 {
		t.Fatalf("history = %d revisions, %v", len(hist), err)
	}
}
