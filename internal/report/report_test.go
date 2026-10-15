package report

import (
	"bytes"
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi/gatewayapitest"
)

func TestCollect(t *testing.T) {
	two := int32(2)
	cs := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "nt"}, Spec: appsv1.DeploymentSpec{Replicas: &two}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-x", Namespace: "nt"}, Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 7, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fine", Namespace: "nt"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "nt"},
			Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "gateway.example.com"}, {Host: "*.llm.example.com"}}},
			Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
				Ingress: []networkingv1.IngressLoadBalancerIngress{{Hostname: "lb.elb.amazonaws.com"}}}},
		},
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "nt"},
			Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "mcp.example.com"}}},
		},
	)
	st, err := Collect(context.Background(), cs, nil, "nt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Ready() {
		t.Error("1/2 replicas reported as ready")
	}
	if len(st.Problems) != 1 || st.Problems[0].Reason != "CrashLoopBackOff" {
		t.Errorf("problems = %+v", st.Problems)
	}
	if len(st.Records) != 3 || st.Records[0].Host != "*.llm.example.com" || st.Records[0].Type != "CNAME" {
		t.Fatalf("records = %+v", st.Records)
	}
	var buf bytes.Buffer
	st.Print(&buf)
	out := buf.String()
	for _, want := range []string{"Workloads: 0/1 ready", "gateway.example.com", "CNAME", "address pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSchedulerReason(t *testing.T) {
	msg := "0/1 nodes are available: 1 Insufficient memory. no new claims to deallocate, preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod."
	if got := schedulerReason(msg); got != "Insufficient memory" {
		t.Errorf("schedulerReason = %q", got)
	}
}

func TestCollectHTTPRouteRecords(t *testing.T) {
	parent := map[string]any{"name": "internal", "namespace": "gw", "sectionName": "https"}
	dyn := gatewayapitest.Client(
		gatewayapitest.Gateway("gw", "internal", "10.0.6.10", gatewayapitest.Listener("https", 443, map[string]any{"from": "All"})),
		gatewayapitest.Route("nt", "agentgateway-gateway", []string{"gateway.h1.example.com", "*.llm.h1.example.com"}, []map[string]any{parent}, nil),
		gatewayapitest.Route("nt", "orphan", []string{"lost.h1.example.com"}, []map[string]any{{"name": "missing"}}, nil),
	)
	st, err := Collect(context.Background(), fake.NewSimpleClientset(), dyn, "nt")
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Host: "*.llm.h1.example.com", Type: "A", Target: "10.0.6.10"},
		{Host: "gateway.h1.example.com", Type: "A", Target: "10.0.6.10"},
		{Host: "lost.h1.example.com"},
	}
	if len(st.Records) != len(want) {
		t.Fatalf("records = %+v", st.Records)
	}
	for i := range want {
		if st.Records[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, st.Records[i], want[i])
		}
	}
}
