// Package diff compares two rendered manifests the way an operator needs to
// see them before an upgrade or a rollback: what is added, removed or
// changed, and which changes Kubernetes cannot apply in place. Secret values
// are compared but never shown.
//
// The blocking rules match the chart's own render-diff gate, so a change the
// chart's CI calls breaking is one ntctl refuses to apply silently.
package diff

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"sigs.k8s.io/yaml"

	"github.com/NeuralTrust/ntctl/internal/manifest"
)

// dataKinds hold data that is lost when the object is deleted.
var dataKinds = map[string]bool{"Secret": true, "PersistentVolumeClaim": true, "StatefulSet": true}

// immutable lists fields Kubernetes rejects changes to, by kind.
var immutable = map[string][][]string{
	"Deployment":            {{"spec", "selector"}},
	"DaemonSet":             {{"spec", "selector"}},
	"StatefulSet":           {{"spec", "selector"}, {"spec", "serviceName"}, {"spec", "volumeClaimTemplates"}},
	"PersistentVolumeClaim": {{"spec", "storageClassName"}, {"spec", "accessModes"}},
}

// volatile labels change with every chart release and say nothing about the
// workload.
var volatile = map[string]bool{"helm.sh/chart": true, "app.kubernetes.io/version": true}

// Op is what happens to a resource.
type Op string

// Operations.
const (
	Added   Op = "added"
	Removed Op = "removed"
	Changed Op = "changed"
)

// Change is one resource that differs.
type Change struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Op   Op     `json:"op"`
	// Blocking says why Kubernetes cannot apply the change in place, or why
	// it loses data. Empty when the change is safe.
	Blocking []string `json:"blocking,omitempty"`
	// Secret keys added, removed or with a new value. Values are never kept.
	SecretKeys *SecretKeys `json:"secretKeys,omitempty"`
	// Unified is the diff of the resource, with Secret values hidden.
	Unified string `json:"-"`
}

// SecretKeys summarises a Secret's change by key.
type SecretKeys struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// Result is every change between two manifests.
type Result struct {
	Changes []Change `json:"changes"`
}

// Blocking returns the changes that cannot be applied safely.
func (r Result) Blocking() []Change {
	var out []Change
	for _, c := range r.Changes {
		if len(c.Blocking) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// Empty reports whether nothing changes.
func (r Result) Empty() bool { return len(r.Changes) == 0 }

// Compare returns what changes when a release goes from one manifest to the
// other.
func Compare(from, to []manifest.Object) Result {
	a, b := index(from), index(to)
	var res Result
	for _, k := range sortedKeys(a, b) {
		oldObj, inOld := a[k]
		newObj, inNew := b[k]
		kind, name := oldObj.Kind(), oldObj.Name()
		if !inOld {
			kind, name = newObj.Kind(), newObj.Name()
		}
		c := Change{Kind: kind, Name: name}
		switch {
		case !inOld:
			c.Op = Added
		case !inNew:
			c.Op = Removed
			if dataKinds[kind] {
				c.Blocking = append(c.Blocking, fmt.Sprintf("removing a %s deletes what it holds", kind))
			}
		default:
			on, nn := normalise(oldObj), normalise(newObj)
			var keys *SecretKeys
			if kind == "Secret" {
				keys = secretKeys(oldObj, newObj)
			}
			if reflect.DeepEqual(on, nn) && keys == nil {
				continue
			}
			c.Op = Changed
			c.SecretKeys = keys
			if keys != nil && len(keys.Removed) > 0 {
				c.Blocking = append(c.Blocking, "removes Secret keys "+strings.Join(keys.Removed, ", ")+" that workloads may read")
			}
			for _, path := range immutable[kind] {
				if !reflect.DeepEqual(dig(on, path), dig(nn, path)) {
					c.Blocking = append(c.Blocking, strings.Join(path, ".")+" is immutable; Kubernetes rejects the change")
				}
			}
			c.Unified = unified(on, nn)
		}
		res.Changes = append(res.Changes, c)
	}
	return res
}

// Print writes a summary grouped by kind, the blocking changes, and with
// full set, each changed resource's diff.
func (r Result) Print(w io.Writer, full bool) {
	if r.Empty() {
		fmt.Fprintln(w, "No resource changes.")
		return
	}
	counts := map[Op]int{}
	for _, c := range r.Changes {
		counts[c.Op]++
	}
	fmt.Fprintf(w, "%d added, %d changed, %d removed:\n", counts[Added], counts[Changed], counts[Removed])
	mark := map[Op]string{Added: "+", Changed: "~", Removed: "-"}
	for _, c := range r.Changes {
		line := fmt.Sprintf("  %s %s/%s", mark[c.Op], c.Kind, c.Name)
		if k := c.SecretKeys; k != nil {
			line += " (" + k.String() + ")"
		}
		if len(c.Blocking) > 0 {
			line += "  BLOCKING"
		}
		fmt.Fprintln(w, line)
	}
	if b := r.Blocking(); len(b) > 0 {
		fmt.Fprintln(w, "\nChanges that cannot be applied safely:")
		for _, c := range b {
			for _, why := range c.Blocking {
				fmt.Fprintf(w, "  %s/%s: %s\n", c.Kind, c.Name, why)
			}
		}
	}
	if !full {
		return
	}
	for _, c := range r.Changes {
		if c.Unified != "" {
			fmt.Fprintf(w, "\n--- %s/%s\n%s", c.Kind, c.Name, c.Unified)
		}
	}
}

func (k *SecretKeys) String() string {
	var parts []string
	if len(k.Added) > 0 {
		parts = append(parts, "keys added: "+strings.Join(k.Added, ", "))
	}
	if len(k.Removed) > 0 {
		parts = append(parts, "keys removed: "+strings.Join(k.Removed, ", "))
	}
	if len(k.Changed) > 0 {
		parts = append(parts, "new values: "+strings.Join(k.Changed, ", "))
	}
	return strings.Join(parts, "; ")
}

func key(o manifest.Object) string {
	ns, _ := dig(map[string]any(o), []string{"metadata", "namespace"}).(string)
	return o.Kind() + "/" + ns + "/" + o.Name()
}

func index(objs []manifest.Object) map[string]manifest.Object {
	out := make(map[string]manifest.Object, len(objs))
	for _, o := range objs {
		out[key(o)] = o
	}
	return out
}

func sortedKeys(a, b map[string]manifest.Object) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]manifest.Object{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// normalise returns a copy without volatile labels or checksum annotations,
// and with Secret values replaced, so only meaningful changes remain.
func normalise(o manifest.Object) map[string]any {
	out, _ := strip(deepCopy(map[string]any(o))).(map[string]any)
	if o.Kind() == "Secret" {
		for _, field := range []string{"data", "stringData"} {
			if d, ok := out[field].(map[string]any); ok {
				for k := range d {
					d[k] = "(hidden)"
				}
			}
		}
	}
	return out
}

func strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if volatile[k] || strings.HasPrefix(k, "checksum/") {
				delete(t, k)
				continue
			}
			t[k] = strip(c)
		}
	case []any:
		for i := range t {
			t[i] = strip(t[i])
		}
	}
	return v
}

// secretKeys compares two Secrets by key and value hash, merging data and
// stringData as the API server does. It returns nil when nothing differs.
func secretKeys(a, b manifest.Object) *SecretKeys {
	av, bv := secretValues(a), secretValues(b)
	k := &SecretKeys{}
	for name, h := range bv {
		old, ok := av[name]
		switch {
		case !ok:
			k.Added = append(k.Added, name)
		case old != h:
			k.Changed = append(k.Changed, name)
		}
	}
	for name := range av {
		if _, ok := bv[name]; !ok {
			k.Removed = append(k.Removed, name)
		}
	}
	if len(k.Added)+len(k.Removed)+len(k.Changed) == 0 {
		return nil
	}
	sort.Strings(k.Added)
	sort.Strings(k.Removed)
	sort.Strings(k.Changed)
	return k
}

func secretValues(o manifest.Object) map[string]string {
	out := map[string]string{}
	if d, ok := o["data"].(map[string]any); ok {
		for k, v := range d {
			s, _ := v.(string)
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				raw = []byte(s)
			}
			out[k] = hash(raw)
		}
	}
	if d, ok := o["stringData"].(map[string]any); ok {
		for k, v := range d {
			s, _ := v.(string)
			out[k] = hash([]byte(s))
		}
	}
	return out
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func unified(a, b map[string]any) string {
	ay, err1 := yaml.Marshal(a)
	by, err2 := yaml.Marshal(b)
	if err1 != nil || err2 != nil {
		return ""
	}
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(string(ay)), B: difflib.SplitLines(string(by)),
		FromFile: "current", ToFile: "target", Context: 3,
	})
	if err != nil {
		return ""
	}
	return out
}

func dig(v any, path []string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, c := range t {
			out[k] = deepCopy(c)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, c := range t {
			out[i] = deepCopy(c)
		}
		return out
	}
	return v
}
