// Package generator knows the chart's in-cluster secrets generator: the hook
// Job that creates the install's generated credentials once and leaves them
// outside the release, so no render rotates them. It tells which Secrets the
// generator owns and which one a rendered release reads from, whatever chart
// version made them.
package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/manifest"
)

const (
	// ManagedBy is the app.kubernetes.io/managed-by value of the Secrets
	// the generator creates.
	ManagedBy = "neuraltrust-secrets-generator"
	// Marker is the annotation the generator sets to "managed" on a Secret
	// it took over from the release that rendered it, and an operator sets
	// to "external" on one the generator must only check.
	Marker = "neuraltrust.ai/secrets-generator"
	// Store is the Secret the generator writes; Legacy is the one charts 3.7
	// to 3.9 wrote, and the prefix of their TLS Secrets, which upgraded
	// installs keep, unread.
	Store  = "platform-secrets"
	Legacy = "neuraltrust-generated"
	// targetEnv is the generator Job's variable naming the Secret it writes,
	// set by every chart version that has the generator.
	targetEnv = "GENERATED_SECRET"
)

// Owns reports whether the generator manages s: it created it, or took it
// over. A Secret an operator supplies (marked external) is not the
// generator's.
func Owns(s *corev1.Secret) bool {
	return s.Labels["app.kubernetes.io/managed-by"] == ManagedBy || s.Annotations[Marker] == "managed"
}

// Owned lists the Secrets in the namespace the generator manages, sorted.
func Owned(ctx context.Context, cs kubernetes.Interface, ns string) ([]string, error) {
	list, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing Secrets: %w", err)
	}
	var out []string
	for i := range list.Items {
		if Owns(&list.Items[i]) {
			out = append(out, list.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Target returns the Secret the generator Job in objs writes, or "" when the
// release renders no generator: it then renders its Secrets itself.
func Target(objs []manifest.Object) string {
	for _, o := range objs {
		if o.Kind() != "Job" {
			continue
		}
		for _, field := range []string{"initContainers", "containers"} {
			list, _ := o.PodSpec()[field].([]any)
			for _, c := range list {
				cm, _ := c.(map[string]any)
				env, _ := cm["env"].([]any)
				for _, e := range env {
					m, _ := e.(map[string]any)
					if m["name"] == targetEnv {
						v, _ := m["value"].(string)
						return v
					}
				}
			}
		}
	}
	return ""
}

// Writes returns every Secret the generator of objs (a release's hooks)
// writes: its Target, and the TLS Secrets its spec names. Empty when the
// release renders no generator.
func Writes(objs []manifest.Object) []string {
	t := Target(objs)
	if t == "" {
		return nil
	}
	out := []string{t}
	// A spec that does not parse names no TLS Secret; Spec's callers that
	// generate from it see the error.
	entries, _ := Spec(objs)
	for _, e := range entries {
		if e.Type == "tls" && e.Secret != "" {
			out = append(out, e.Secret)
		}
	}
	return out
}

// Entry is one entry of the generator's spec: a key of its Target, or, for
// type tls, a TLS Secret of its own.
type Entry struct {
	Key    string `json:"key"`
	Type   string `json:"type"`
	Length int    `json:"length,omitempty"`
	// Source and Format make an alias (= source) or a derived value (Format
	// with %s replaced by the source).
	Source string `json:"source,omitempty"`
	Format string `json:"format,omitempty"`
	// Adopt lists [secret, key] pairs an earlier chart rendered the value
	// into; FreshIf, for type install, the Secrets whose presence means the
	// install predates the key.
	Adopt   [][]string `json:"adopt,omitempty"`
	FreshIf []string   `json:"freshIf,omitempty"`
	// TLS: the Secret, its subject and names, and its issuer.
	Secret     string   `json:"secret,omitempty"`
	CN         string   `json:"cn,omitempty"`
	SANs       []string `json:"sans,omitempty"`
	Days       int      `json:"days,omitempty"`
	SelfSigned bool     `json:"selfSigned,omitempty"`
	CACN       string   `json:"caCn,omitempty"`
}

// specPrefix names the hook ConfigMaps each component contributes its
// entries in.
const specPrefix = "neuraltrust-generator-spec-"

// Spec returns the entries of every generator spec ConfigMap in objs, in
// the order the release renders them.
func Spec(objs []manifest.Object) ([]Entry, error) {
	var out []Entry
	for _, o := range objs {
		if o.Kind() != "ConfigMap" || !strings.HasPrefix(o.Name(), specPrefix) {
			continue
		}
		data, _ := o["data"].(map[string]any)
		raw, _ := data["spec.json"].(string)
		var entries []Entry
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("reading the generator spec %s: %w", o.Name(), err)
		}
		out = append(out, entries...)
	}
	return out, nil
}
