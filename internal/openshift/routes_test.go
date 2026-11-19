package openshift

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestWildcard(t *testing.T) {
	t.Parallel()
	const router = "router-default.apps.cluster.example.com"
	cases := []struct {
		host, target, want string
	}{
		{"app.apps.cluster.example.com", router, "*.apps.cluster.example.com"},
		{"trustguard.apps.cluster.example.com", "router-internal.apps.cluster.example.com", "*.apps.cluster.example.com"},
		{"a.b.apps.cluster.example.com", router, ""},
		{"app.example.com", router, ""},
		{"app.apps.cluster.example.com", "lb.elb.amazonaws.com", ""},
		{"app.apps.cluster.example.com", "", ""},
		{"*.apps.cluster.example.com", router, ""},
	}
	for _, c := range cases {
		if got := Wildcard(c.host, c.target); got != c.want {
			t.Errorf("Wildcard(%q, %q) = %q, want %q", c.host, c.target, got, c.want)
		}
	}
}

// Object is a Route for tests; admitted "" leaves it with no status.
func object(name, host, admitted string) *unstructured.Unstructured {
	o := map[string]any{
		"apiVersion": "route.openshift.io/v1", "kind": "Route",
		"metadata": map[string]any{"name": name, "namespace": "nt"},
		"spec":     map[string]any{"host": host},
	}
	if admitted != "" {
		o["status"] = map[string]any{"ingress": []any{map[string]any{
			"host": host, "routerName": "default", "routerCanonicalHostname": "router-default.apps.example.com",
			"conditions": []any{map[string]any{"type": "Admitted", "status": admitted, "reason": "HostAlreadyClaimed", "message": "another route claims it"}},
		}}}
	}
	return &unstructured.Unstructured{Object: o}
}

func TestListRoutes(t *testing.T) {
	t.Parallel()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{RoutesGVR: "RouteList"})
	for _, o := range []*unstructured.Unstructured{
		object("app", "app.apps.example.com", "True"),
		object("api", "api.apps.example.com", "False"),
		object("new", "new.apps.example.com", ""),
	} {
		if err := dyn.Tracker().Create(RoutesGVR, o, "nt"); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := ListRoutes(context.Background(), dyn, "nt")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Route{}
	for _, r := range routes {
		got[r.Name] = r
	}
	if r := got["app"]; !r.Admitted || r.Router != "router-default.apps.example.com" || r.Reason != "" {
		t.Errorf("admitted route = %+v", r)
	}
	if r := got["api"]; r.Admitted || r.Reason != "HostAlreadyClaimed: another route claims it" {
		t.Errorf("refused route = %+v", r)
	}
	if r := got["new"]; r.Admitted || r.Reason == "" {
		t.Errorf("route with no status = %+v", r)
	}
}
