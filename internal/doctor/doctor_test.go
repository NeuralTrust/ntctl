package doctor

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi/gatewayapitest"
	"github.com/NeuralTrust/ntctl/internal/preflight"
)

const ns = "neuraltrust"

func pod(name, rs string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs}}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}, ImagePullSecrets: []corev1.LocalObjectReference{{Name: "gcr-secret"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mutate(p)
	return p
}

func waiting(reason, msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Image: "registry.example.com/app:1", RestartCount: 7,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
	}
}

func rel(status rcommon.Status, manifest string) *release.Release {
	return &release.Release{
		Name: "neuraltrust-platform", Version: 3, Namespace: ns, Manifest: manifest,
		Chart:  &chartv2.Chart{Metadata: &chartv2.Metadata{Name: "neuraltrust-platform", Version: "3.0.2"}},
		Info:   &release.Info{Status: status, Description: "Upgrade \"neuraltrust-platform\" failed: context deadline exceeded"},
		Config: map[string]any{"global": map[string]any{"platform": "kubernetes"}},
	}
}

func run(t *testing.T, r *release.Release, objs ...runtime.Object) map[string]preflight.Result {
	t.Helper()
	cs := fake.NewClientset(objs...)
	rs, err := Run(context.Background(), Input{
		Clientset: cs, Namespace: ns, Release: "neuraltrust-platform", Current: r,
		Cluster: &detect.Cluster{Version: "v1.34.0", Platform: "kubernetes", Nodes: 1, StorageClasses: []string{"standard"}, DefaultStorageClass: "standard"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]preflight.Result{}
	for _, r := range rs {
		if _, dup := out[r.Code]; dup && strings.HasPrefix(r.Code, "NT-DR-01") {
			t.Errorf("finding %s repeated; pods of one workload should be grouped: %+v", r.Code, r)
		}
		out[r.Code] = r
	}
	return out
}

func TestRuntimeFindings(t *testing.T) {
	sc := "fast"
	oom := pod("worker-abc-1", "worker-abc", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
	})
	got := run(t, rel(rcommon.StatusDeployed, ""),
		pod("proxy-7d9f-a", "proxy-7d9f", waiting("ImagePullBackOff", "Back-off pulling image: failed to authorize: 401 Unauthorized")),
		pod("proxy-7d9f-b", "proxy-7d9f", waiting("ImagePullBackOff", "Back-off pulling image: failed to authorize: 401 Unauthorized")),
		pod("mcp-9a1-a", "mcp-9a1", waiting("ErrImagePull", "failed to pull: 401 Unauthorized")),
		pod("mcp-55c-a", "mcp-55c", waiting("CreateContainerConfigError", `secret "missing-redis" not found`)),
		pod("api-6b8f-a", "api-6b8f", waiting("CrashLoopBackOff", "")),
		oom,
		pod("db-0", "db", func(p *corev1.Pod) {
			p.OwnerReferences[0].Kind = "StatefulSet"
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Message: "0/1 nodes are available: 1 Insufficient memory. preemption: 0/1 nodes are available"}}
		}),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "db-bootstrap", Namespace: ns},
			Status: batchv1.JobStatus{Failed: 6, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns}},
	)

	want := map[string]string{
		"NT-DR-001": "release neuraltrust-platform revision 3, chart 3.0.2, deployed",
		"NT-DR-010": "2 workloads cannot pull their images: the registry refused the credentials",
		"NT-DR-011": `mcp cannot start container app: secret "missing-redis" not found`,
		"NT-DR-012": "api container app keeps crashing",
		"NT-DR-013": "db cannot be scheduled: 1 Insufficient memory",
		"NT-DR-014": "worker container app was killed for exceeding its memory limit",
		"NT-DR-020": "volume data-db-0 is not bound (StorageClass fast)",
		"NT-DR-021": "job db-bootstrap failed (6 attempts)",
		"NT-DR-022": "1 of 1 ingresses have no address yet: gateway",
	}
	for code, msg := range want {
		r, ok := got[code]
		if !ok {
			t.Errorf("%s missing", code)
			continue
		}
		if !strings.Contains(r.Message, msg) {
			t.Errorf("%s message = %q, want it to contain %q", code, r.Message, msg)
		}
		if code != "NT-DR-001" && r.Fix == "" {
			t.Errorf("%s has no fix", code)
		}
	}
	if d := got["NT-DR-010"].Detail; len(d) != 2 || d[0] != "mcp: registry.example.com/app:1" || d[1] != "proxy: registry.example.com/app:1" {
		t.Errorf("pull detail should list each workload once with its image, got %v", d)
	}
	if !strings.Contains(got["NT-DR-010"].Fix, "gcr-secret") {
		t.Errorf("the pull finding should name the pull Secret: %q", got["NT-DR-010"].Fix)
	}
	if d := got["NT-DR-012"].Detail; len(d) == 0 || d[0] != "fake logs" {
		t.Errorf("a crash finding should carry the last log lines, got %v", d)
	}
	if !preflight.Failed(mapValues(got)) {
		t.Error("these findings should fail the run")
	}
}

func TestReleaseStates(t *testing.T) {
	if r := run(t, nil)["NT-DR-001"]; r.Severity != preflight.Warn || !strings.Contains(r.Message, "no release") {
		t.Errorf("no release: %+v", r)
	}
	r := run(t, rel(rcommon.StatusFailed, ""))["NT-DR-001"]
	if r.Severity != preflight.Fail || len(r.Detail) == 0 || !strings.Contains(r.Detail[0], "deadline") {
		t.Errorf("failed release: %+v", r)
	}
	r = run(t, rel(rcommon.StatusPendingUpgrade, ""))["NT-DR-001"]
	if r.Severity != preflight.Fail || !strings.Contains(r.Fix, "helm rollback") {
		t.Errorf("stuck release: %+v", r)
	}
}

func TestDeployedSecretGoneMissing(t *testing.T) {
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: proxy
spec:
  template:
    spec:
      containers:
      - name: app
        envFrom:
        - secretRef:
            name: agentgateway-config-sync
`
	got := run(t, rel(rcommon.StatusDeployed, manifest))
	r := got["NT-PF-006"]
	if r.Severity != preflight.Fail || !strings.Contains(r.Message, "agentgateway-config-sync") {
		t.Fatalf("a Secret the deployed release reads but is gone should fail: %+v", r)
	}
	if _, ok := got["NT-PF-008"]; ok {
		t.Error("the capacity check compares with free room as if nothing ran; it does not belong in doctor")
	}
}

func TestHealthyInstall(t *testing.T) {
	healthy := pod("proxy-1-a", "proxy-1", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	})
	got := run(t, rel(rcommon.StatusDeployed, ""), healthy)
	if preflight.Failed(mapValues(got)) {
		t.Fatalf("a healthy install should pass: %+v", got)
	}
}

func TestLogLinesAreRedacted(t *testing.T) {
	r := preflight.Result{Detail: []string{"dial postgres://nt:CANARY@db/nt failed"}}
	r.Detail[0] = clip(r.Detail[0], 300)
	if strings.Contains(r.Detail[0], "CANARY") {
		t.Fatalf("a log line kept a password: %s", r.Detail[0])
	}
}

func mapValues(m map[string]preflight.Result) []preflight.Result {
	out := make([]preflight.Result, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out
}

func TestPullCauses(t *testing.T) {
	p := corev1.Pod{}
	for msg, want := range map[string]string{
		`failed to resolve reference "r/x:1": r/x:1: not found`: "the image or tag is not in that registry",
		"dial tcp: lookup registry.example.com: no such host":   "the nodes cannot reach the registry",
		"pull access denied, repository does not exist":         "the registry refused the credentials",
		"something new": "the pull fails (something new)",
	} {
		if got, _ := pullCauseOf("ErrImagePull", msg, p); got != want {
			t.Errorf("pullCauseOf(%q) = %q, want %q", msg, got, want)
		}
	}
	if got, _ := pullCauseOf("InvalidImageName", "", p); got != "the image name is invalid" {
		t.Errorf("InvalidImageName = %q", got)
	}
}

func TestHTTPRouteNotAccepted(t *testing.T) {
	parent := map[string]any{"name": "internal", "namespace": "gw"}
	dyn := gatewayapitest.Client(
		gatewayapitest.Route(ns, "app", []string{"app.example.com"}, []map[string]any{parent},
			[]map[string]any{gatewayapitest.ParentStatus(parent, "Accepted", "True", "Accepted")}),
		gatewayapitest.Route(ns, "trustguard", []string{"trustguard.example.com"}, []map[string]any{parent},
			[]map[string]any{gatewayapitest.ParentStatus(parent, "ResolvedRefs", "False", "BackendNotFound")}),
	)
	rs, err := Run(context.Background(), Input{
		Clientset: fake.NewClientset(), Dynamic: dyn, Namespace: ns, Release: "neuraltrust-platform", Current: rel(rcommon.StatusDeployed, ""),
		Cluster: &detect.Cluster{Version: "v1.34.0", Platform: "kubernetes", Nodes: 1, StorageClasses: []string{"standard"}, DefaultStorageClass: "standard"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found *preflight.Result
	for i := range rs {
		if rs[i].Code == "NT-DR-023" {
			found = &rs[i]
		}
	}
	if found == nil {
		t.Fatalf("no NT-DR-023 for a route the Gateway refused: %+v", rs)
	}
	if !strings.Contains(found.Message, "1 of 2 HTTPRoutes") || !strings.Contains(found.Message, "trustguard") ||
		len(found.Detail) != 1 || !strings.Contains(found.Detail[0], "BackendNotFound") {
		t.Errorf("finding = %+v", found)
	}
}
