// Package gatewayapi reads the Gateway API objects an install attaches to:
// Gateways with their listeners and addresses, and a namespace's HTTPRoutes.
// It goes through the dynamic client, so ntctl carries no Gateway API types,
// and it only reads.
package gatewayapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Group is the Gateway API group.
const Group = "gateway.networking.k8s.io"

// GVRs of the resources read here.
var (
	GatewaysGVR   = schema.GroupVersionResource{Group: Group, Version: "v1", Resource: "gateways"}
	HTTPRoutesGVR = schema.GroupVersionResource{Group: Group, Version: "v1", Resource: "httproutes"}
)

// Listener is one Gateway listener and which namespaces may attach routes to it.
type Listener struct {
	Name     string
	Protocol string
	Port     int64
	Hostname string
	// From is allowedRoutes.namespaces.from: All, Same or Selector.
	From     string
	selector labels.Selector
}

// Admits reports whether a route in namespace ns, labelled nsLabels, may
// attach to the listener of a Gateway in gatewayNS.
func (l Listener) Admits(gatewayNS, ns string, nsLabels map[string]string) bool {
	switch l.From {
	case "All":
		return true
	case "Selector":
		return l.selector != nil && l.selector.Matches(labels.Set(nsLabels))
	default: // Same, the API default
		return ns == gatewayNS
	}
}

// Address is one published Gateway address.
type Address struct {
	Type  string // IPAddress or Hostname
	Value string
}

// Gateway is what an install needs to know about a Gateway.
type Gateway struct {
	Namespace string
	Name      string
	Class     string
	Listeners []Listener
	Addresses []Address
}

// Ref is namespace/name.
func (g Gateway) Ref() string { return g.Namespace + "/" + g.Name }

// Listener returns the named listener.
func (g Gateway) Listener(name string) (Listener, bool) {
	for _, l := range g.Listeners {
		if l.Name == name {
			return l, true
		}
	}
	return Listener{}, false
}

// DNS returns the record type and target that route hostnames on this
// Gateway resolve to, or empty strings while it has no address.
func (g Gateway) DNS() (typ, target string) {
	for _, a := range g.Addresses {
		switch a.Type {
		case "Hostname":
			return "CNAME", a.Value
		case "IPAddress", "":
			return "A", a.Value
		}
	}
	return "", ""
}

// ParentRef names a Gateway a route attaches to. An empty Namespace means the
// route's own namespace.
type ParentRef struct {
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	SectionName string `json:"sectionName,omitempty"`
}

// In resolves the namespace against the route's own.
func (p ParentRef) In(routeNS string) ParentRef {
	if p.Namespace == "" {
		p.Namespace = routeNS
	}
	return p
}

// String is namespace/name[:section].
func (p ParentRef) String() string {
	s := p.Namespace + "/" + p.Name
	if p.SectionName != "" {
		s += ":" + p.SectionName
	}
	return s
}

// ParseRef reads namespace/name[:section], the form of ntctl's --gateway.
func ParseRef(s string) (ParentRef, error) {
	ref := strings.TrimSpace(s)
	var p ParentRef
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		p.SectionName, ref = ref[i+1:], ref[:i]
	}
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") || (strings.Contains(s, ":") && p.SectionName == "") {
		return ParentRef{}, fmt.Errorf("gateway %q: want <namespace>/<name> or <namespace>/<name>:<listener>", s)
	}
	p.Namespace, p.Name = ns, name
	return p, nil
}

// Condition is one status condition a controller reported.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// ParentStatus is a controller's verdict on one parent of a route.
type ParentStatus struct {
	Parent     ParentRef   `json:"parentRef"`
	Controller string      `json:"controllerName"`
	Conditions []Condition `json:"conditions"`
}

// Route is what an install needs to know about an HTTPRoute.
type Route struct {
	Namespace string
	Name      string
	Hostnames []string
	Parents   []ParentRef
	Status    []ParentStatus
}

type gatewayObject struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		GatewayClassName string `json:"gatewayClassName"`
		Listeners        []struct {
			Name          string `json:"name"`
			Protocol      string `json:"protocol"`
			Port          int64  `json:"port"`
			Hostname      string `json:"hostname"`
			AllowedRoutes struct {
				Namespaces struct {
					From     string                `json:"from"`
					Selector *metav1.LabelSelector `json:"selector"`
				} `json:"namespaces"`
			} `json:"allowedRoutes"`
		} `json:"listeners"`
	} `json:"spec"`
	Status struct {
		Addresses []Address `json:"addresses"`
	} `json:"status"`
}

type routeObject struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		Hostnames  []string    `json:"hostnames"`
		ParentRefs []ParentRef `json:"parentRefs"`
	} `json:"spec"`
	Status struct {
		Parents []ParentStatus `json:"parents"`
	} `json:"status"`
}

func decode(u *unstructured.Unstructured, into any) error {
	b, err := json.Marshal(u.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func toGateway(u *unstructured.Unstructured) (Gateway, error) {
	var o gatewayObject
	if err := decode(u, &o); err != nil {
		return Gateway{}, fmt.Errorf("reading Gateway %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	g := Gateway{Namespace: o.Metadata.Namespace, Name: o.Metadata.Name, Class: o.Spec.GatewayClassName, Addresses: o.Status.Addresses}
	for _, l := range o.Spec.Listeners {
		ln := Listener{Name: l.Name, Protocol: l.Protocol, Port: l.Port, Hostname: l.Hostname, From: l.AllowedRoutes.Namespaces.From}
		if ln.From == "" {
			ln.From = "Same"
		}
		if ln.From == "Selector" && l.AllowedRoutes.Namespaces.Selector != nil {
			sel, err := metav1.LabelSelectorAsSelector(l.AllowedRoutes.Namespaces.Selector)
			if err != nil {
				return Gateway{}, fmt.Errorf("Gateway %s listener %s: %w", g.Ref(), l.Name, err)
			}
			ln.selector = sel
		}
		g.Listeners = append(g.Listeners, ln)
	}
	return g, nil
}

// ListGateways returns every Gateway in the cluster. A cluster without the
// Gateway API returns none and no error.
func ListGateways(ctx context.Context, dyn dynamic.Interface) ([]Gateway, error) {
	list, err := dyn.Resource(GatewaysGVR).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing Gateways: %w", err)
	}
	out := make([]Gateway, 0, len(list.Items))
	for i := range list.Items {
		g, err := toGateway(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// GetGateway returns one Gateway, or nil when it does not exist.
func GetGateway(ctx context.Context, dyn dynamic.Interface, ns, name string) (*Gateway, error) {
	u, err := dyn.Resource(GatewaysGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Gateway %s/%s: %w", ns, name, err)
	}
	g, err := toGateway(u)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ListRoutes returns the HTTPRoutes in a namespace, with parent namespaces
// resolved. A cluster without the Gateway API returns none and no error.
func ListRoutes(ctx context.Context, dyn dynamic.Interface, ns string) ([]Route, error) {
	list, err := dyn.Resource(HTTPRoutesGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing HTTPRoutes: %w", err)
	}
	out := make([]Route, 0, len(list.Items))
	for i := range list.Items {
		var o routeObject
		if err := decode(&list.Items[i], &o); err != nil {
			return nil, fmt.Errorf("reading HTTPRoute %s: %w", list.Items[i].GetName(), err)
		}
		r := Route{Namespace: o.Metadata.Namespace, Name: o.Metadata.Name, Hostnames: o.Spec.Hostnames, Status: o.Status.Parents}
		for _, p := range o.Spec.ParentRefs {
			r.Parents = append(r.Parents, p.In(r.Namespace))
		}
		for j := range r.Status {
			r.Status[j].Parent = r.Status[j].Parent.In(r.Namespace)
		}
		out = append(out, r)
	}
	return out, nil
}

// Problems lists, for each parent, the conditions a controller reported as
// not True among Accepted and ResolvedRefs, as "parent: Type Reason: message".
// A route no controller has reported on yet yields one line saying so.
func (r Route) Problems() []string {
	if len(r.Status) == 0 {
		return []string{"no Gateway controller has reported on it yet"}
	}
	var out []string
	for _, ps := range r.Status {
		for _, c := range ps.Conditions {
			if (c.Type == "Accepted" || c.Type == "ResolvedRefs") && c.Status != "True" {
				line := fmt.Sprintf("%s: %s=%s", ps.Parent, c.Type, c.Status)
				if c.Reason != "" {
					line += " " + c.Reason
				}
				if c.Message != "" {
					line += ": " + c.Message
				}
				out = append(out, line)
			}
		}
	}
	return out
}
