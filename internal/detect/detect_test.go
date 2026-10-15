package detect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi/gatewayapitest"
)

func node(name, provider string, cpu, mem string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: provider, Taints: taints},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}},
	}
}

func TestInspect(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("w1", "aws:///eu-west-1a/i-1", "4", "16Gi"),
		node("gpu", "aws:///eu-west-1a/i-2", "8", "32Gi", corev1.Taint{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}),
		node("cp", "aws:///eu-west-1a/i-3", "2", "8Gi", corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}),
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx", Annotations: map[string]string{defaultIngressAnnotation: "true"}}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3", Annotations: map[string]string{defaultStorageAnnotation: "true"}}},
	)
	c, err := Inspect(context.Background(), cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Platform != "aws" {
		t.Errorf("platform = %q, want aws", c.Platform)
	}
	if c.Nodes != 2 || c.CPUMilli != 12000 {
		t.Errorf("nodes=%d cpu=%dm; want the GPU pool counted and the control plane left out", c.Nodes, c.CPUMilli)
	}
	if c.DefaultIngressClass != "nginx" || c.DefaultStorageClass != "gp3" {
		t.Errorf("defaults: ingress=%q storage=%q", c.DefaultIngressClass, c.DefaultStorageClass)
	}
}

func TestInspectOpenShift(t *testing.T) {
	cs := fake.NewSimpleClientset(node("w1", "gce://p/z/n", "4", "16Gi"))
	cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: "route.openshift.io/v1"}}
	c, err := Inspect(context.Background(), cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Platform != "openshift" || !c.HasRoutes {
		t.Errorf("platform = %q routes=%v, want openshift", c.Platform, c.HasRoutes)
	}
}

func TestPlatformFallback(t *testing.T) {
	if p := platform(false, []corev1.Node{{Spec: corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"}}}); p != "kubernetes" {
		t.Errorf("platform = %q, want kubernetes", p)
	}
}

func TestInspectGatewayAPI(t *testing.T) {
	cs := fake.NewSimpleClientset(node("w1", "azure:///subscriptions/x", "4", "16Gi"))
	cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: "gateway.networking.k8s.io/v1"}}
	dyn := gatewayapitest.Client(gatewayapitest.Gateway("kgateway-system", "internal", "10.0.6.10", gatewayapitest.Listener("https", 443, nil)))
	c, err := Inspect(context.Background(), cs, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if !c.GatewayAPI || !c.GatewaysRead || len(c.Gateways) != 1 || c.Gateways[0].Ref() != "kgateway-system/internal" {
		t.Errorf("gateway api=%v read=%v gateways=%+v", c.GatewayAPI, c.GatewaysRead, c.Gateways)
	}

	// Without the API group, Gateways are not even listed.
	plain := fake.NewSimpleClientset(node("w1", "azure:///subscriptions/x", "4", "16Gi"))
	c, err = Inspect(context.Background(), plain, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if c.GatewayAPI || c.GatewaysRead || len(c.Gateways) != 0 {
		t.Errorf("cluster without Gateway API: api=%v read=%v gateways=%+v", c.GatewayAPI, c.GatewaysRead, c.Gateways)
	}
}

func TestNamespaceLabels(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "nt", Labels: map[string]string{"team": "ai"}}})
	got, err := NamespaceLabels(context.Background(), cs, "nt")
	if err != nil || got["team"] != "ai" || got["kubernetes.io/metadata.name"] != "nt" {
		t.Errorf("existing namespace labels = %v, %v", got, err)
	}
	got, err = NamespaceLabels(context.Background(), cs, "new")
	if err != nil || len(got) != 1 || got["kubernetes.io/metadata.name"] != "new" {
		t.Errorf("a namespace not created yet = %v, %v; want only the name label", got, err)
	}
}

func TestInspectCountsRunningPods(t *testing.T) {
	pod := func(ns, name, node, phase string, cpu string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("1Gi")}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodPhase(phase)},
		}
	}
	cs := fake.NewSimpleClientset(
		node("w1", "aws:///eu-west-1a/i-1", "4", "16Gi"),
		pod("kube-system", "ds", "w1", "Running", "250m"),
		pod("kube-system", "dns", "w1", "Running", "100m"),
		pod("neuraltrust", "done", "w1", "Succeeded", "2"),
	)
	c, err := Inspect(context.Background(), cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.UsageRead || len(c.NodeList) != 1 {
		t.Fatalf("usage read = %v, nodes = %d", c.UsageRead, len(c.NodeList))
	}
	u := c.NodeList[0].Used
	if u["kube-system"].CPUMilli != 350 || u["kube-system"].Memory != 2<<30 {
		t.Errorf("kube-system usage = %+v", u["kube-system"])
	}
	if _, ok := u["neuraltrust"]; ok {
		t.Error("a finished pod must not count")
	}
}

func TestInspectDetectsAutoscaler(t *testing.T) {
	cs := fake.NewSimpleClientset(node("w1", "aws:///eu-west-1a/i-1", "4", "16Gi"))
	if c, _ := Inspect(context.Background(), cs, nil); c.Autoscaling {
		t.Fatal("no autoscaler here")
	}
	cs = fake.NewSimpleClientset(node("w1", "aws:///eu-west-1a/i-1", "4", "16Gi"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cluster-autoscaler-status"}})
	if c, _ := Inspect(context.Background(), cs, nil); !c.Autoscaling {
		t.Fatal("cluster-autoscaler-status should mark the cluster as autoscaling")
	}
}
