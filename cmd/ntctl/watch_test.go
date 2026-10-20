package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/report"
)

func TestHopelessFor(t *testing.T) {
	crash := func(pods string) preflight.Result {
		return preflight.Result{Code: "NT-DR-012", Severity: preflight.Fail, Message: "api container app keeps crashing" + pods}
	}
	unschedulable := preflight.Result{Code: "NT-DR-013", Severity: preflight.Fail, Message: "db cannot be scheduled"}
	since := map[string]time.Time{}
	t0 := time.Now()
	if got := hopelessFor([]preflight.Result{crash(""), unschedulable}, since, t0, time.Minute); len(got) != 0 {
		t.Fatalf("first sight should only start the clock: %v", got)
	}
	// The pod count changing does not restart the clock.
	if got := hopelessFor([]preflight.Result{crash(" [2 pods]")}, since, t0.Add(30*time.Second), time.Minute); len(got) != 0 {
		t.Fatalf("not lasted long enough yet: %v", got)
	}
	if got := hopelessFor([]preflight.Result{crash(" [2 pods]"), unschedulable}, since, t0.Add(time.Minute), time.Minute); len(got) != 1 || got[0].Code != "NT-DR-012" {
		t.Fatalf("a crash loop lasting the limit should be returned, and only it: %v", got)
	}
	// Cleared once: the clock starts over.
	hopelessFor(nil, since, t0.Add(2*time.Minute), time.Minute)
	if got := hopelessFor([]preflight.Result{crash("")}, since, t0.Add(3*time.Minute), time.Minute); len(got) != 0 {
		t.Fatalf("a finding that cleared must start over: %v", got)
	}
}

func TestProgress(t *testing.T) {
	st := &report.Status{Workloads: []report.Workload{
		{Name: "redis", Ready: 1, Desired: 1}, {Name: "proxy", Ready: 0, Desired: 2}, {Name: "mcp", Ready: 1, Desired: 2},
	}}
	if got := progress(st); got != "1/3 workloads ready; waiting for mcp, proxy" {
		t.Errorf("progress = %q", got)
	}
	var many []report.Workload
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		many = append(many, report.Workload{Name: n, Desired: 1})
	}
	if got := progress(&report.Status{Workloads: many}); !strings.HasSuffix(got, "waiting for a, b, c, d, and 2 more") {
		t.Errorf("progress = %q", got)
	}
}

// TestWatchStopsOnALastingCrashLoop runs the watcher against a fake cluster
// with a crash-looping pod: it reports progress, then stops the wait.
func TestWatchStopsOnALastingCrashLoop(t *testing.T) {
	two := int32(2)
	cs := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "nt"}, Spec: appsv1.DeploymentSpec{Replicas: &two}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-6b8f-a", Namespace: "nt", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-6b8f"}}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 6,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}},
		},
	)
	var out bytes.Buffer
	w := waitWatch{cs: cs, cluster: &detect.Cluster{Version: "v1.34.0", Platform: "kubernetes"}, ns: "nt", release: "r", out: &out,
		every: 5 * time.Millisecond, giveUp: 20 * time.Millisecond, stopEarly: true}.start(context.Background())
	select {
	case <-w.stop.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the wait was not stopped")
	}
	found := w.finish()
	if len(found) == 0 || found[0].Code != "NT-DR-012" {
		t.Fatalf("findings = %v", found)
	}
	for _, want := range []string{"0/1 workloads ready; waiting for api", "Stopping the wait: api container app keeps crashing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Without stopEarly it only reports, until finished.
	out.Reset()
	w = waitWatch{cs: cs, ns: "nt", out: &out, every: 5 * time.Millisecond, giveUp: time.Nanosecond}.start(context.Background())
	time.Sleep(50 * time.Millisecond)
	if w.stop.Err() != nil {
		t.Fatal("stopped the wait without stopEarly")
	}
	if found := w.finish(); len(found) != 0 {
		t.Fatalf("findings without stopEarly: %v", found)
	}
}
