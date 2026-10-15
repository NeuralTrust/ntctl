// Package gatewayapitest builds Gateway API objects and a fake dynamic client
// that serves them, for tests.
package gatewayapitest

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
)

// Client serves the objects through a fake dynamic client. Objects are added
// under their real resource: the fake's own guess pluralises Gateway as
// "gatewaies".
func Client(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayapi.GatewaysGVR:   "GatewayList",
		gatewayapi.HTTPRoutesGVR: "HTTPRouteList",
	})
	for _, o := range objs {
		gvr := gatewayapi.HTTPRoutesGVR
		if o.GetKind() == "Gateway" {
			gvr = gatewayapi.GatewaysGVR
		}
		if err := dyn.Tracker().Create(gvr, o, o.GetNamespace()); err != nil {
			panic(err)
		}
	}
	return dyn
}

// Listener is a listener spec: name, port and allowedRoutes.namespaces.
func Listener(name string, port int64, namespaces map[string]any) map[string]any {
	l := map[string]any{"name": name, "port": port, "protocol": "HTTPS"}
	if namespaces != nil {
		l["allowedRoutes"] = map[string]any{"namespaces": namespaces}
	}
	return l
}

// Gateway is a Gateway with an IP address (none when ip is empty).
func Gateway(ns, name, ip string, listeners ...map[string]any) *unstructured.Unstructured {
	ls := make([]any, 0, len(listeners))
	for _, l := range listeners {
		ls = append(ls, l)
	}
	obj := map[string]any{
		"apiVersion": gatewayapi.Group + "/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec":       map[string]any{"gatewayClassName": "test", "listeners": ls},
	}
	if ip != "" {
		obj["status"] = map[string]any{"addresses": []any{map[string]any{"type": "IPAddress", "value": ip}}}
	}
	return &unstructured.Unstructured{Object: obj}
}

// Route is an HTTPRoute with hostnames and parent refs; parents is a list of
// maps such as {"name": "internal", "namespace": "gw"}. status, when not nil,
// becomes status.parents.
func Route(ns, name string, hosts []string, parents []map[string]any, status []map[string]any) *unstructured.Unstructured {
	hs := make([]any, 0, len(hosts))
	for _, h := range hosts {
		hs = append(hs, h)
	}
	ps := make([]any, 0, len(parents))
	for _, p := range parents {
		ps = append(ps, p)
	}
	obj := map[string]any{
		"apiVersion": gatewayapi.Group + "/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec":       map[string]any{"hostnames": hs, "parentRefs": ps},
	}
	if status != nil {
		st := make([]any, 0, len(status))
		for _, s := range status {
			st = append(st, s)
		}
		obj["status"] = map[string]any{"parents": st}
	}
	return &unstructured.Unstructured{Object: obj}
}

// ParentStatus is a status.parents entry with one condition per type=status
// pair, e.g. ParentStatus(parent, "Accepted", "False", "NotAllowedByListeners").
func ParentStatus(parent map[string]any, condType, status, reason string) map[string]any {
	return map[string]any{
		"parentRef":      parent,
		"controllerName": "example.com/gateway",
		"conditions": []any{map[string]any{
			"type": condType, "status": status, "reason": reason, "message": reason + " message",
		}},
	}
}
