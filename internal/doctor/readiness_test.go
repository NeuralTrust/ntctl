package doctor

import (
	"strings"
	"testing"
	"time"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NeuralTrust/ntctl/internal/preflight"
)

// running is a container that started ago and is not ready.
func running(ago time.Duration) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CONFIG_SYNC_GRPC_ENDPOINT", Value: "agentgateway-configsync.example.com:443"}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-ago))}}}}
	}
}

func TestRunningButNotReady(t *testing.T) {
	probe := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "proxy-1-a.1", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "proxy-1-a"},
		Type:           corev1.EventTypeWarning, Reason: "Unhealthy", Message: "Readiness probe failed: HTTP probe failed with statuscode: 503",
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "other.1", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "other"},
		Type:           corev1.EventTypeWarning, Reason: "Unhealthy", Message: "Readiness probe failed: not this pod",
	}
	got := run(t, rel(rcommon.StatusFailed, ""),
		pod("proxy-1-a", "proxy-1", running(20*time.Minute)), pod("proxy-1-b", "proxy-1", running(20*time.Minute)), probe, other)
	r, ok := got["NT-DR-015"]
	if !ok {
		t.Fatalf("no NT-DR-015 for pods running unready for 20 minutes: %+v", got)
	}
	if r.Severity != preflight.Fail || !strings.Contains(r.Message, "proxy container app runs but never becomes ready [2 pods]") {
		t.Errorf("finding = %+v", r)
	}
	if len(r.Detail) == 0 || !strings.Contains(r.Detail[0], "statuscode: 503") {
		t.Errorf("detail should start with the probe failure: %v", r.Detail)
	}
	for _, d := range r.Detail {
		if strings.Contains(d, "not this pod") {
			t.Errorf("another pod's event leaked in: %v", r.Detail)
		}
	}
	if !strings.Contains(got["NT-DR-001"].Fix, "findings below") {
		t.Errorf("NT-DR-001 should now point at the findings: %q", got["NT-DR-001"].Fix)
	}

	// Just started: not a finding yet.
	if _, ok := run(t, rel(rcommon.StatusDeployed, ""), pod("proxy-1-a", "proxy-1", running(10*time.Second)))["NT-DR-015"]; ok {
		t.Error("a container still starting is not a finding")
	}
}

func TestNotableLines(t *testing.T) {
	logs := []string{
		`{"time":"t1","level":"INFO","msg":"starting"}`,
		`{"time":"t2","level":"WARN","msg":"catch-up converge after stream break failed","error":"configsync: fetch snapshot: rpc error: code = Unauthenticated"}`,
		`{"time":"t3","level":"ERROR","msg":"change stream watch failed","error":"configsync: watch recv: rpc error: code = Unauthenticated"}`,
		`{"time":"t4","level":"WARN","msg":"catch-up converge after stream break failed","error":"configsync: fetch snapshot: rpc error: code = Unauthenticated"}`,
		`{"time":"t5","level":"ERROR","msg":"change stream watch failed","error":"configsync: watch recv: rpc error: code = Unauthenticated"}`,
		`plain text line`,
		``,
	}
	got := notableLines(logs, 10)
	want := []string{
		"WARN catch-up converge after stream break failed: configsync: fetch snapshot: rpc error: code = Unauthenticated",
		"ERROR change stream watch failed: configsync: watch recv: rpc error: code = Unauthenticated",
		"plain text line",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notableLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := notableLines(logs, 1); len(got) != 1 || got[0] != "plain text line" {
		t.Errorf("the last n: %v", got)
	}
}

func TestKnownCause(t *testing.T) {
	ep := map[string]string{"CONFIG_SYNC_GRPC_ENDPOINT": "agentgateway-configsync.neuraltrust.ai:443"}
	cases := map[string]string{
		"ERROR change stream watch failed: configsync: watch recv: rpc error: code = Unauthenticated desc = missing or invalid config-sync token": "Config sync at agentgateway-configsync.neuraltrust.ai:443 refuses this data plane's token",
		`FATAL: password authentication failed for user "nt"`:  "PostgreSQL refuses",
		"WRONGPASS invalid username-password pair":             "Redis refuses",
		"x509: certificate signed by unknown authority":        "TLS certificate is not trusted",
		"dial tcp: lookup db.example.com: no such host":        "does not resolve",
		"dial tcp 192.0.2.1:5432: connect: connection refused": "cannot be reached",
		"all good": "",
	}
	for line, want := range cases {
		got := knownCause([]string{line}, ep)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("knownCause(%q) = %q, want it to contain %q", line, got, want)
		}
	}
}

// TestNotableLinesReadsLongLines: real log lines carry a source block that
// takes them past the printed width; they must be parsed whole.
func TestNotableLinesReadsLongLines(t *testing.T) {
	long := `{"time":"2026-10-20T15:49:46.11367691Z","level":"ERROR","source":{"function":"` + strings.Repeat("x", 400) +
		`","file":"worker.go","line":296},"msg":"change stream watch failed","component":"configsync","error":"rpc error: code = Unauthenticated"}`
	got := notableLines([]string{long}, 5)
	if len(got) != 1 || got[0] != "ERROR change stream watch failed: rpc error: code = Unauthenticated" {
		t.Fatalf("notableLines = %q", got)
	}
}

func TestHopeless(t *testing.T) {
	fail := func(code, fix string) preflight.Result {
		return preflight.Result{Code: code, Severity: preflight.Fail, Fix: fix}
	}
	cases := map[string]struct {
		r    preflight.Result
		want bool
	}{
		"image pull":             {fail("NT-DR-010", "x"), true},
		"missing Secret":         {fail("NT-DR-011", "x"), true},
		"crash loop":             {fail("NT-DR-012", "x"), true},
		"unready, known cause":   {fail("NT-DR-015", "Config sync at … refuses"), true},
		"unready, unknown cause": {fail("NT-DR-015", readinessFix), false},
		"unschedulable":          {fail("NT-DR-013", "x"), false},
		"failed release":         {fail("NT-DR-001", "x"), false},
		"a warning":              {preflight.Result{Code: "NT-DR-010", Severity: preflight.Warn}, false},
	}
	for name, c := range cases {
		if got := Hopeless(c.r); got != c.want {
			t.Errorf("%s: Hopeless = %v, want %v", name, got, c.want)
		}
	}
}
