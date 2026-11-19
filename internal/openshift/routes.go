// Package openshift reads OpenShift Routes (route.openshift.io/v1): the hosts
// an install answers on, and whether a router admitted them.
package openshift

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// RoutesGVR is the resource of OpenShift Routes.
var RoutesGVR = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}

// Route is one OpenShift Route, its own or one the router made from an
// Ingress.
type Route struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// Router is the canonical hostname of the router that admitted the
	// route, such as router-default.apps.example.com: where its DNS points.
	Router string `json:"router,omitempty"`
	// Admitted reports whether a router admitted the route.
	Admitted bool `json:"admitted"`
	// Reason is why no router admitted it, empty when one did.
	Reason string `json:"reason,omitempty"`
}

// ListRoutes returns the Routes in namespace ns. A cluster without the Route
// API, or one that does not let the caller list Routes, has none to report.
func ListRoutes(ctx context.Context, dyn dynamic.Interface, ns string) ([]Route, error) {
	list, err := dyn.Resource(RoutesGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing Routes: %w", err)
	}
	out := make([]Route, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, fromObject(&list.Items[i]))
	}
	return out, nil
}

func fromObject(u *unstructured.Unstructured) Route {
	r := Route{Name: u.GetName()}
	r.Host, _, _ = unstructured.NestedString(u.Object, "spec", "host")
	ingress, _, _ := unstructured.NestedSlice(u.Object, "status", "ingress")
	if len(ingress) == 0 {
		r.Reason = "no router has reported on it yet"
		return r
	}
	var refused []string
	for _, in := range ingress {
		m, ok := in.(map[string]any)
		if !ok {
			continue
		}
		router, _ := m["routerCanonicalHostname"].(string)
		conds, _ := m["conditions"].([]any)
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok || cm["type"] != "Admitted" {
				continue
			}
			if cm["status"] == "True" {
				r.Admitted, r.Router, r.Reason = true, router, ""
				return r
			}
			reason, _ := cm["reason"].(string)
			msg, _ := cm["message"].(string)
			refused = append(refused, strings.TrimSpace(reason+": "+msg))
		}
	}
	r.Reason = "not admitted"
	if len(refused) > 0 {
		r.Reason = strings.Join(refused, "; ")
	}
	return r
}

// Wildcard returns the wildcard DNS name, *.<domain>, under which an
// OpenShift router at target already answers for host, or "" when host needs
// its own record. A router's canonical hostname is router-<name>.<domain>,
// and the cluster's ingress domain has wildcard DNS pointing at the router:
// a host one label under that domain resolves already.
func Wildcard(host, target string) string {
	name, domain, ok := strings.Cut(target, ".")
	if !ok || !strings.HasPrefix(name, "router-") || domain == "" {
		return ""
	}
	label, rest, ok := strings.Cut(host, ".")
	if !ok || label == "" || label == "*" || rest != domain {
		return ""
	}
	return "*." + domain
}
