package doctor

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi/gatewayapitest"
	"github.com/NeuralTrust/ntctl/internal/preflight"
)

const sccMessage = `Error creating: pods "neuraltrust-platform-secrets-generator-" is forbidden: unable to validate against any security context constraint: [provider "anyuid": Forbidden: not usable by user or serviceaccount, provider restricted-v2: .containers[0].runAsUser: Invalid value: 1001: must be in the ranges: [1001070000, 1001079999], provider "nonroot": Forbidden: not usable by user or serviceaccount]`

func failedCreate(kind, name, msg string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name + ".1", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: ns},
		Type:           corev1.EventTypeWarning, Reason: "FailedCreate", Message: msg,
	}
}

func generatorJob(active int32) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "neuraltrust-platform-secrets-generator", Namespace: ns},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			ServiceAccountName: "neuraltrust-platform-secrets-generator", Containers: []corev1.Container{{Name: "generate"}},
		}}},
		Status: batchv1.JobStatus{Active: active},
	}
}

func TestAdmissionRefused(t *testing.T) {
	one := int32(1)
	refusedRS := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api-6b8f", Namespace: ns},
		Spec:       appsv1.ReplicaSetSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}},
	}
	cases := []struct {
		name     string
		rel      *release.Release
		objs     []runtime.Object
		want     bool
		contains []string
	}{
		{
			name: "a pre-install hook refused by an SCC, during the install",
			rel:  rel(rcommon.StatusPendingInstall, ""),
			objs: []runtime.Object{generatorJob(0), failedCreate("Job", "neuraltrust-platform-secrets-generator", sccMessage)},
			want: true,
			contains: []string{
				"job neuraltrust-platform-secrets-generator", "service account neuraltrust-platform-secrets-generator",
				"oc adm policy add-scc-to-user nonroot-v2 -z neuraltrust-platform-secrets-generator -n " + ns, "restricted-v2: .containers[0].runAsUser",
			},
		},
		{
			name: "a ReplicaSet refused by Pod Security, with no release yet",
			objs: []runtime.Object{refusedRS, failedCreate("ReplicaSet", "api-6b8f", `Error creating: pods "api-6b8f-x" is forbidden: violates PodSecurity "restricted:latest": seccompProfile`)},
			want: true, contains: []string{"replicaset api-6b8f", "service account default", "Pod Security"},
		},
		{
			name: "a Job that has its pods since is no longer refused",
			rel:  rel(rcommon.StatusPendingInstall, ""),
			objs: []runtime.Object{generatorJob(1), failedCreate("Job", "neuraltrust-platform-secrets-generator", sccMessage)},
		},
		{
			name: "the event of a Job that is gone is ignored",
			rel:  rel(rcommon.StatusDeployed, ""),
			objs: []runtime.Object{failedCreate("Job", "neuraltrust-platform-secrets-generator", sccMessage)},
		},
		{
			name: "another FailedCreate cause is not an admission refusal",
			rel:  rel(rcommon.StatusDeployed, ""),
			objs: []runtime.Object{generatorJob(0), failedCreate("Job", "neuraltrust-platform-secrets-generator", `Error creating: pods "x" is forbidden: exceeded quota: compute`)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := run(t, c.rel, c.objs...)["NT-DR-016"]
			if ok != c.want {
				t.Fatalf("NT-DR-016 reported = %v, want %v: %+v", ok, c.want, r)
			}
			if !ok {
				return
			}
			if r.Severity != preflight.Fail || !Hopeless(r) {
				t.Errorf("an admission refusal is a failure waiting does not fix: %+v", r)
			}
			all := r.Message + "\n" + r.Fix + "\n" + strings.Join(r.Detail, "\n")
			for _, s := range c.contains {
				if !strings.Contains(all, s) {
					t.Errorf("finding does not mention %q:\n%s", s, all)
				}
			}
		})
	}
}

func TestPendingInstallPointsAtTheCause(t *testing.T) {
	r := run(t, rel(rcommon.StatusPendingInstall, ""))["NT-DR-001"]
	if !strings.HasPrefix(r.Fix, "An install still running continues on its own once the cause") {
		t.Errorf("a pending install should point at the cause before uninstalling: %q", r.Fix)
	}
}

func TestDeployedChecksCountHookWrittenSecrets(t *testing.T) {
	r := rel(rcommon.StatusDeployed, `apiVersion: apps/v1
kind: Deployment
metadata: {name: app}
spec:
  template:
    spec:
      containers:
      - name: app
        envFrom: [{secretRef: {name: platform-secrets}}]
`)
	r.Hooks = []*release.Hook{{Manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: neuraltrust-platform-secrets-generator
  annotations: {helm.sh/hook: "pre-install,pre-upgrade"}
rules:
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [platform-secrets]
  verbs: [get, create, update]
`}}
	if got := run(t, r)["NT-PF-006"]; got.Severity == preflight.Fail {
		t.Errorf("a Secret the secrets generator writes is not one the operator must create: %+v", got)
	}
}

func TestOpenShiftRouteNotAdmitted(t *testing.T) {
	dyn := gatewayapitest.Client(
		gatewayapitest.OpenShiftRoute(ns, "app", "app.apps.example.com", "True", ""),
		gatewayapitest.OpenShiftRoute(ns, "trustguard", "trustguard.apps.example.com", "False", "HostAlreadyClaimed"),
	)
	rs, err := Run(context.Background(), Input{
		Clientset: fake.NewClientset(), Dynamic: dyn, Namespace: ns, Release: "neuraltrust-platform", Current: rel(rcommon.StatusDeployed, ""),
		Cluster: &detect.Cluster{Version: "v1.34.0", Platform: "openshift", Nodes: 1, StorageClasses: []string{"standard"}, DefaultStorageClass: "standard", HasRoutes: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found *preflight.Result
	for i := range rs {
		if rs[i].Code == "NT-DR-026" {
			found = &rs[i]
		}
	}
	if found == nil {
		t.Fatalf("no NT-DR-026 for a Route the router refused: %+v", rs)
	}
	if !strings.Contains(found.Message, "1 of 2 OpenShift Routes") || len(found.Detail) != 1 || !strings.Contains(found.Detail[0], "HostAlreadyClaimed") {
		t.Errorf("finding = %+v", found)
	}
}
