// Package manifest reads a rendered release and reports what it needs from the
// cluster, so preflight checks follow the chart rather than duplicating it.
package manifest

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
)

// Object is one rendered document.
type Object map[string]any

// apiVersion returns the object's apiVersion.
func apiVersion(o Object) string { s, _ := o["apiVersion"].(string); return s }

// Kind returns the object's kind.
func (o Object) Kind() string { s, _ := o["kind"].(string); return s }

// Name returns metadata.name.
func (o Object) Name() string { s, _ := dig(o, "metadata", "name").(string); return s }

// Parse splits a rendered manifest stream into objects.
func Parse(rendered string) ([]Object, error) {
	var out []Object
	for _, doc := range strings.Split("\n"+rendered, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var o Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			return nil, fmt.Errorf("parsing rendered manifest: %w", err)
		}
		if o != nil && o.Kind() != "" {
			out = append(out, o)
		}
	}
	return out, nil
}

// SecretRef is a Secret a workload depends on.
type SecretRef struct {
	Name string
	Key  string // empty when the whole Secret is used
	By   string // Kind/name of the first object that references it
}

// Needs summarises what a release requires from the cluster.
type Needs struct {
	// Secrets referenced by workloads that the release does not create itself.
	Secrets []SecretRef
	// PullSecrets named in imagePullSecrets that the release does not create.
	PullSecrets []string
	// StorageClasses requested explicitly; DefaultStorage is true when any
	// claim relies on the cluster default.
	StorageClasses []string
	DefaultStorage bool
	// IngressClasses requested explicitly; DefaultIngress is true when any
	// Ingress relies on the cluster default.
	IngressClasses []string
	DefaultIngress bool
	Routes         bool
	// HTTPRoutes is true when the release renders Gateway API HTTPRoutes;
	// Gateways are the parents they name, with the namespace left empty when
	// it is the release's own.
	HTTPRoutes bool
	Gateways   []gatewayapi.ParentRef
	// Requested CPU (millicores) and memory (bytes), replicas included.
	CPUMilli int64
	Memory   int64
	// Groups split those requests by where the pods may run: workloads with
	// the same nodeSelector and tolerations share a group.
	Groups []PodGroup
	// LoadBalancers are the Services of type LoadBalancer the release
	// renders, which the cloud gives an address of their own.
	LoadBalancers []LoadBalancer
	// Dials are the control-plane addresses (host:port) a data plane dials
	// from inside the cluster, such as its config-sync and DataBridge
	// endpoints. Cluster-local names are left out.
	Dials []string
}

// dialKeys are the environment variables and ConfigMap keys that carry a
// control-plane address a data plane dials.
var dialKeys = map[string]bool{"CONFIG_SYNC_GRPC_ENDPOINT": true, "DATABRIDGE_ADDR": true}

// external reports whether addr names a host outside the cluster: a dotted
// DNS name that is neither cluster-local nor an IP address.
func external(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return strings.Contains(host, ".") && net.ParseIP(host) == nil &&
		!strings.HasSuffix(host, ".svc") && !strings.HasSuffix(host, ".cluster.local") && !strings.Contains(host, ".svc.")
}

// LoadBalancer is a Service of type LoadBalancer and its annotations, which
// decide on most clouds whether its address is private.
type LoadBalancer struct {
	Name        string
	Annotations map[string]string
}

// PodGroup is the requests of workloads that can run on the same nodes.
type PodGroup struct {
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	Workloads    []string
	// CPUMilli and Memory are the group's total requests; MaxPodCPUMilli and
	// MaxPodMemory those of its largest single pod, which must fit one node.
	CPUMilli       int64
	Memory         int64
	MaxPodCPUMilli int64
	MaxPodMemory   int64
}

var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true, "CronJob": true}

// Analyse walks the objects and collects their needs.
func Analyse(objs []Object) Needs {
	created := map[string]bool{}
	for _, o := range objs {
		if o.Kind() == "Secret" {
			created[o.Name()] = true
		}
	}
	var n Needs
	secrets := map[string]SecretRef{}
	pull := map[string]bool{}
	storage := map[string]bool{}
	ingress := map[string]bool{}
	gateways := map[gatewayapi.ParentRef]bool{}
	groups := map[string]*PodGroup{}
	var groupOrder []string
	dials := map[string]bool{}
	dial := func(key string, v any) {
		if addr, _ := v.(string); dialKeys[key] && external(strings.TrimSpace(addr)) {
			dials[strings.TrimSpace(addr)] = true
		}
	}

	for _, o := range objs {
		kind := o.Kind()
		by := kind + "/" + o.Name()
		switch {
		case kind == "PersistentVolumeClaim":
			noteClass(dig(o, "spec", "storageClassName"), storage, &n.DefaultStorage)
		case kind == "Ingress":
			noteClass(dig(o, "spec", "ingressClassName"), ingress, &n.DefaultIngress)
		case kind == "Route":
			n.Routes = true
		case kind == "ConfigMap":
			if d, ok := o["data"].(map[string]any); ok {
				for k, v := range d {
					dial(k, v)
				}
			}
		case kind == "Service" && dig(o, "spec", "type") == "LoadBalancer":
			lb := LoadBalancer{Name: o.Name(), Annotations: map[string]string{}}
			if m, ok := dig(o, "metadata", "annotations").(map[string]any); ok {
				for k, v := range m {
					lb.Annotations[k] = fmt.Sprint(v)
				}
			}
			n.LoadBalancers = append(n.LoadBalancers, lb)
		case kind == "HTTPRoute" && strings.HasPrefix(apiVersion(o), gatewayapi.Group+"/"):
			n.HTTPRoutes = true
			for _, p := range list(dig(o, "spec", "parentRefs")) {
				group, _ := dig(p, "group").(string)
				pk, _ := dig(p, "kind").(string)
				if (group != "" && group != gatewayapi.Group) || (pk != "" && pk != "Gateway") {
					continue
				}
				ref := gatewayapi.ParentRef{}
				ref.Name, _ = dig(p, "name").(string)
				ref.Namespace, _ = dig(p, "namespace").(string)
				ref.SectionName, _ = dig(p, "sectionName").(string)
				if !gateways[ref] {
					gateways[ref] = true
					n.Gateways = append(n.Gateways, ref)
				}
			}
		case workloadKinds[kind]:
			pod := podSpec(o)
			if pod == nil {
				continue
			}
			for _, vct := range list(dig(o, "spec", "volumeClaimTemplates")) {
				noteClass(dig(vct, "spec", "storageClassName"), storage, &n.DefaultStorage)
			}
			for _, ps := range list(pod["imagePullSecrets"]) {
				if name, _ := dig(ps, "name").(string); name != "" && !created[name] {
					pull[name] = true
				}
			}
			for _, v := range list(pod["volumes"]) {
				name, _ := dig(v, "secret", "secretName").(string)
				optional, _ := dig(v, "secret", "optional").(bool)
				addSecret(secrets, created, name, "", optional, by)
			}
			replicas := int64(1)
			if kind == "Deployment" || kind == "StatefulSet" {
				if r, ok := dig(o, "spec", "replicas").(float64); ok {
					replicas = int64(r)
				}
			}
			var podCPU, podMem int64
			for _, field := range []string{"initContainers", "containers"} {
				for _, c := range list(pod[field]) {
					for _, e := range list(dig(c, "env")) {
						if k, _ := dig(e, "name").(string); k != "" {
							dial(k, dig(e, "value"))
						}
						name, _ := dig(e, "valueFrom", "secretKeyRef", "name").(string)
						key, _ := dig(e, "valueFrom", "secretKeyRef", "key").(string)
						optional, _ := dig(e, "valueFrom", "secretKeyRef", "optional").(bool)
						addSecret(secrets, created, name, key, optional, by)
					}
					for _, e := range list(dig(c, "envFrom")) {
						name, _ := dig(e, "secretRef", "name").(string)
						optional, _ := dig(e, "secretRef", "optional").(bool)
						addSecret(secrets, created, name, "", optional, by)
					}
					if field == "containers" && kind != "Job" && kind != "CronJob" {
						podCPU += quantity(dig(c, "resources", "requests", "cpu"), true)
						podMem += quantity(dig(c, "resources", "requests", "memory"), false)
					}
				}
			}
			if podCPU+podMem > 0 {
				n.CPUMilli += replicas * podCPU
				n.Memory += replicas * podMem
				g := group(groups, &groupOrder, pod)
				g.Workloads = append(g.Workloads, by)
				g.CPUMilli += replicas * podCPU
				g.Memory += replicas * podMem
				g.MaxPodCPUMilli = max(g.MaxPodCPUMilli, podCPU)
				g.MaxPodMemory = max(g.MaxPodMemory, podMem)
			}
		}
	}
	for _, r := range secrets {
		n.Secrets = append(n.Secrets, r)
	}
	sort.Slice(n.Secrets, func(i, j int) bool {
		if n.Secrets[i].Name != n.Secrets[j].Name {
			return n.Secrets[i].Name < n.Secrets[j].Name
		}
		return n.Secrets[i].Key < n.Secrets[j].Key
	})
	for _, k := range groupOrder {
		n.Groups = append(n.Groups, *groups[k])
	}
	n.Dials = keys(dials)
	n.PullSecrets = keys(pull)
	n.StorageClasses = keys(storage)
	n.IngressClasses = keys(ingress)
	return n
}

func addSecret(acc map[string]SecretRef, created map[string]bool, name, key string, optional bool, by string) {
	if name == "" || optional || created[name] {
		return
	}
	id := name + "/" + key
	if _, seen := acc[id]; !seen {
		acc[id] = SecretRef{Name: name, Key: key, By: by}
	}
}

func noteClass(v any, acc map[string]bool, usesDefault *bool) {
	if s, _ := v.(string); s != "" {
		acc[s] = true
		return
	}
	*usesDefault = true
}

// group returns the PodGroup for a pod's nodeSelector and tolerations,
// creating it on first use.
func group(groups map[string]*PodGroup, order *[]string, pod map[string]any) *PodGroup {
	var g PodGroup
	if sel, ok := pod["nodeSelector"].(map[string]any); ok && len(sel) > 0 {
		g.NodeSelector = map[string]string{}
		for k, v := range sel {
			g.NodeSelector[k] = fmt.Sprint(v)
		}
	}
	if raw, ok := pod["tolerations"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &g.Tolerations) // a malformed toleration is skipped, as the scheduler would reject it
		}
	}
	keyBytes, _ := json.Marshal([]any{g.NodeSelector, g.Tolerations}) // map keys marshal sorted
	key := string(keyBytes)
	if existing, ok := groups[key]; ok {
		return existing
	}
	groups[key] = &g
	*order = append(*order, key)
	return &g
}

// PodSpec is the pod template's spec of a workload, or nil for other kinds.
func (o Object) PodSpec() map[string]any { return podSpec(o) }

func podSpec(o Object) map[string]any {
	if o.Kind() == "CronJob" {
		m, _ := dig(o, "spec", "jobTemplate", "spec", "template", "spec").(map[string]any)
		return m
	}
	m, _ := dig(o, "spec", "template", "spec").(map[string]any)
	return m
}

func quantity(v any, cpu bool) int64 {
	s := fmt.Sprint(v)
	if v == nil || s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	if cpu {
		return q.MilliValue()
	}
	return q.Value()
}

func dig(v any, path ...string) any {
	for _, p := range path {
		if o, ok := v.(Object); ok {
			v = map[string]any(o)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func list(v any) []any { l, _ := v.([]any); return l }

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
