// Package manifest reads a rendered release and reports what it needs from the
// cluster, so preflight checks follow the chart rather than duplicating it.
package manifest

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// Object is one rendered document.
type Object map[string]any

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
	// Requested CPU (millicores) and memory (bytes), replicas included.
	CPUMilli int64
	Memory   int64
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
			for _, field := range []string{"initContainers", "containers"} {
				for _, c := range list(pod[field]) {
					for _, e := range list(dig(c, "env")) {
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
						n.CPUMilli += replicas * quantity(dig(c, "resources", "requests", "cpu"), true)
						n.Memory += replicas * quantity(dig(c, "resources", "requests", "memory"), false)
					}
				}
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
