package preflight

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/manifest"
)

const gi = int64(1) << 30

func cnode(name string, cpuMilli, mem int64, labels map[string]string, taints ...corev1.Taint) detect.Node {
	return detect.Node{Name: name, Labels: labels, Taints: taints, CPUMilli: cpuMilli, Memory: mem, Used: map[string]detect.Usage{}}
}

var (
	gpuTaint    = corev1.Taint{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}
	systemTaint = corev1.Taint{Key: "CriticalAddonsOnly", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	gpuTol      = corev1.Toleration{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}
)

func general(cpuMilli, mem int64) manifest.PodGroup {
	return manifest.PodGroup{Workloads: []string{"Deployment/a", "Deployment/b", "Deployment/c"}, CPUMilli: cpuMilli, Memory: mem,
		Pods: []manifest.Pod{{Workload: "Deployment/a", CPUMilli: 1000, Memory: 2 * gi}}}
}

func capIn(nodes []detect.Node, groups ...manifest.PodGroup) Input {
	in := base()
	in.Cluster.NodeList = nodes
	in.Cluster.UsageRead = true
	in.Needs.Groups = groups
	return in
}

func TestCapacityLeavesOutNodesTheReleaseCannotUse(t *testing.T) {
	// Two 8 vCPU / 30 GiB workers, an AKS system pool and a GPU node. Only the
	// workers can take general workloads: 12 vCPU of 16 is 75%, fits.
	nodes := []detect.Node{
		cnode("w1", 8000, 30*gi, nil), cnode("w2", 8000, 30*gi, nil),
		cnode("sys1", 2000, 6*gi, nil, systemTaint), cnode("sys2", 2000, 6*gi, nil, systemTaint),
		cnode("gpu1", 4000, 16*gi, map[string]string{"accelerator": "nvidia"}, gpuTaint),
	}
	r := capacity(capIn(nodes, general(12000, 20*gi)))
	if r.Severity != OK || !strings.Contains(r.Message, "3 nodes left out") {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}
	if !strings.Contains(strings.Join(r.Detail, "\n"), "on 2 nodes: needs 12.0 of 16.0 vCPU free") {
		t.Errorf("detail should count only the two workers: %v", r.Detail)
	}
	// The same requests against all five nodes' totals would have passed at
	// 50%; against the usable ones 14 vCPU is 88%, which warns.
	if r := capacity(capIn(nodes, general(14000, 20*gi))); r.Severity != Warn || !strings.Contains(r.Message, "CPU for") {
		t.Fatalf("88%% of the usable CPU should warn: %s %s", r.Severity, r.Message)
	}
	if r := capacity(capIn(nodes, general(17000, 20*gi))); r.Severity != Fail {
		t.Fatalf("more than the usable CPU should fail: %s %s", r.Severity, r.Message)
	}
}

func TestCapacityGPUWorkersUseTheGPUPool(t *testing.T) {
	nodes := []detect.Node{
		cnode("w1", 8000, 30*gi, nil),
		cnode("gpu1", 4000, 16*gi, map[string]string{"accelerator": "nvidia"}, gpuTaint),
	}
	gpu := manifest.PodGroup{NodeSelector: map[string]string{"accelerator": "nvidia"}, Tolerations: []corev1.Toleration{gpuTol},
		Workloads: []string{"Deployment/w"}, CPUMilli: 3000, Memory: 12 * gi,
		Pods: []manifest.Pod{{Workload: "Deployment/w", CPUMilli: 1000, Memory: 4 * gi}}}
	r := capacity(capIn(nodes, general(4000, 10*gi), gpu))
	if r.Severity != OK {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}
	// Without a GPU node the GPU workers fit nowhere.
	r = capacity(capIn(nodes[:1], general(4000, 10*gi), gpu))
	if r.Severity != Fail || !strings.Contains(r.Message, "no node can run workloads selecting accelerator=nvidia") {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}
}

func TestCapacitySubtractsRunningPods(t *testing.T) {
	w := cnode("w1", 8000, 30*gi, nil)
	w.Used["kube-system"] = detect.Usage{CPUMilli: 1500, Memory: 2 * gi}
	w.Used["neuraltrust"] = detect.Usage{CPUMilli: 5000, Memory: 10 * gi} // the release itself, on an upgrade
	// 5 vCPU needed: of 6.5 free after kube-system is 77%, fits. Counting the
	// release's own pods as overhead too would leave 1.5 and fail.
	r := capacity(capIn([]detect.Node{w}, general(5000, 10*gi)))
	if r.Severity != OK {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}
	if !strings.Contains(r.Detail[0], "of 6.5 vCPU free") {
		t.Errorf("free CPU should leave out kube-system only: %v", r.Detail)
	}
}

func TestCapacityLargestPodMustFitANode(t *testing.T) {
	nodes := []detect.Node{cnode("w1", 4000, 16*gi, nil), cnode("w2", 4000, 16*gi, nil)}
	g := general(6000, 20*gi)
	g.Pods[0].Memory = 20 * gi
	in := capIn(nodes, g)
	in.IgnoreCapacity = true
	// Bigger than any node in total: no flag or autoscaler helps.
	if r := capacity(in); r.Severity != Fail || !strings.Contains(r.Message, "bigger than any of its nodes") {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}
	// Fits a node's total, but no node has that much free now: a shortfall.
	nodes[0].Used["other"] = detect.Usage{Memory: 10 * gi}
	nodes[1].Used["other"] = detect.Usage{Memory: 10 * gi}
	g.Pods[0].Memory = 8 * gi
	g.Memory = 8 * gi
	in = capIn(nodes, g)
	if r := capacity(in); r.Severity != Fail || !strings.Contains(r.Message, "fits no node's free room") {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}
	in.IgnoreCapacity = true
	if r := capacity(in); r.Severity != Warn {
		t.Fatalf("--ignore-capacity should downgrade a free-room shortfall: %s %s", r.Severity, r.Message)
	}
}

func TestCapacityPodNeedsCPUAndMemoryOnTheSameNode(t *testing.T) {
	// One node has the CPU, the other the memory; neither has both. Taking
	// the largest CPU and the largest memory separately would let it pass.
	nodes := []detect.Node{cnode("cpu", 8000, 8*gi, nil), cnode("mem", 2000, 32*gi, nil)}
	g := general(4000, 16*gi)
	g.Pods = []manifest.Pod{{Workload: "Deployment/firewall", CPUMilli: 4000, Memory: 16 * gi}}
	in := capIn(nodes, g)
	in.IgnoreCapacity = true
	r := capacity(in)
	if r.Severity != Fail || !strings.Contains(r.Message, "a pod of Deployment/firewall (4.0 vCPU, 16.0 GiB) is bigger than any of its nodes") ||
		!strings.Contains(r.Message, "2.0 vCPU / 32.0 GiB, 8.0 vCPU / 8.0 GiB") {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}

	// The same on free room: both nodes are large enough in total, but other
	// pods leave the CPU free on one and the memory free on the other.
	nodes = []detect.Node{cnode("a", 8000, 32*gi, nil), cnode("b", 8000, 32*gi, nil)}
	nodes[0].Used["other"] = detect.Usage{Memory: 24 * gi}
	nodes[1].Used["other"] = detect.Usage{CPUMilli: 6000}
	r = capacity(capIn(nodes, g))
	if r.Severity != Fail || !strings.Contains(r.Message, "fits no node's free room") {
		t.Fatalf("got %s: %s", r.Severity, r.Message)
	}

	// Two pods that each fit a different node pass.
	g.Pods = []manifest.Pod{{Workload: "Deployment/cpu", CPUMilli: 6000, Memory: 4 * gi}, {Workload: "Deployment/mem", CPUMilli: 1000, Memory: 20 * gi}}
	g.CPUMilli, g.Memory = 7000, 24*gi
	if r := capacity(capIn([]detect.Node{cnode("cpu", 8000, 8*gi, nil), cnode("mem", 2000, 32*gi, nil)}, g)); r.Severity == Fail {
		t.Fatalf("each pod fits a node: %s %s", r.Severity, r.Message)
	}
}

func TestCapacityReportsUnreadUsage(t *testing.T) {
	in := capIn([]detect.Node{cnode("w1", 8000, 30*gi, nil)}, general(2000, 4*gi))
	in.Cluster.UsageRead = false
	if r := capacity(in); !strings.Contains(r.Message, "could not be read") {
		t.Fatalf("message should say running pods were not counted: %s", r.Message)
	}
}

func TestTolerates(t *testing.T) {
	cases := []struct {
		tols  []corev1.Toleration
		taint corev1.Taint
		want  bool
	}{
		{nil, gpuTaint, false},
		{[]corev1.Toleration{gpuTol}, gpuTaint, true},
		{[]corev1.Toleration{{Operator: corev1.TolerationOpExists}}, systemTaint, true},
		{[]corev1.Toleration{{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpEqual, Value: "false"}}, systemTaint, false},
		{[]corev1.Toleration{{Key: "CriticalAddonsOnly", Value: "true"}}, systemTaint, true},
		{nil, corev1.Taint{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}, true},
	}
	for i, c := range cases {
		if got := tolerates(c.tols, []corev1.Taint{c.taint}); got != c.want {
			t.Errorf("case %d: tolerates = %v, want %v", i, got, c.want)
		}
	}
}

func TestCapacityShortfallWarnsWhenNodesCanBeAdded(t *testing.T) {
	nodes := []detect.Node{cnode("w1", 4000, 16*gi, nil)}
	in := capIn(nodes, general(6000, 10*gi))
	if r := capacity(in); r.Severity != Fail || !strings.Contains(r.Fix, "--ignore-capacity") {
		t.Fatalf("without an autoscaler: %s %s / %s", r.Severity, r.Message, r.Fix)
	}
	in.Cluster.Autoscaling = true
	if r := capacity(in); r.Severity != Warn || !strings.Contains(r.Message, "autoscaler should add nodes") {
		t.Fatalf("with an autoscaler: %s %s", r.Severity, r.Message)
	}
	in.Cluster.Autoscaling = false
	in.IgnoreCapacity = true
	if r := capacity(in); r.Severity != Warn || !strings.Contains(r.Message, "--ignore-capacity") {
		t.Fatalf("with --ignore-capacity: %s %s", r.Severity, r.Message)
	}
	// A pool with no node at all for some workload is never downgraded.
	gpu := manifest.PodGroup{NodeSelector: map[string]string{"accelerator": "nvidia"}, Workloads: []string{"Deployment/w"}, CPUMilli: 1000, Memory: gi}
	if r := capacity(capIn(nodes, general(1000, gi), gpu)); r.Severity != Fail {
		t.Fatalf("no matching node must still fail: %s", r.Message)
	}
}
