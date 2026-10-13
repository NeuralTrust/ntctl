// Package bundle writes a support bundle: a tar.gz holding what NeuralTrust
// support needs to diagnose an install, with secrets removed. It collects the
// release's values and manifest, the namespace's resources, events and recent
// logs, node capacity, versions and a doctor report. Every file goes through
// the redaction in package redact, and any text equal to a value stored in a
// Secret of the namespace is masked wherever it appears. The operator can
// inspect the archive before sending it.
package bundle

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/redact"
)

// Input is what goes into the bundle.
type Input struct {
	Clientset kubernetes.Interface
	Namespace string
	Release   string
	// Current and History are the release, nil and empty when there is none.
	Current  *release.Release
	History  []*release.Release
	Findings []preflight.Result
	// Versions are shown first in the bundle: ntctl, Kubernetes, chart.
	Versions map[string]string
	// LogLines is how many lines of each container's log are kept.
	LogLines int64
	Now      time.Time
}

// Summary describes a written bundle.
type Summary struct {
	Files int
	// Errors are what could not be collected, e.g. for lack of permission.
	// They are also written to errors.txt in the bundle.
	Errors []string
}

// minSecretLen is the shortest Secret value scrubbed everywhere it appears;
// shorter ones (ports, "true") would mask unrelated text.
const minSecretLen = 6

// looksRandom reports whether a Secret value under an unremarkable key name
// still looks like a credential: long, with letters and digits, and none of
// the punctuation of a host name, path or URL. A user or host name such as
// "neuraltrust" or "db.example.com" stays visible; it is needed to diagnose.
func looksRandom(v []byte) bool {
	if len(v) < 20 || bytes.ContainsAny(v, ".:/@ \n") {
		return false
	}
	return bytes.ContainsAny(v, "0123456789") && bytes.ContainsAny(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

type writer struct {
	tw      *tar.Writer
	now     time.Time
	known   [][]byte
	summary *Summary
}

// Write collects the bundle into w.
func Write(ctx context.Context, w io.Writer, in Input) (*Summary, error) {
	if in.LogLines == 0 {
		in.LogLines = 500
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	gz := gzip.NewWriter(w)
	bw := &writer{tw: tar.NewWriter(gz), now: in.Now, summary: &Summary{}}
	cs, ns := in.Clientset, in.Namespace

	// Secrets: their names and key names go in; their values are kept only
	// in memory, to be scrubbed from everything else.
	var secretList []map[string]any
	if list, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{}); err != nil {
		bw.fail("secrets", err)
	} else {
		for _, s := range list.Items {
			keys := make([]string, 0, len(s.Data))
			for k, v := range s.Data {
				keys = append(keys, k)
				if len(v) >= minSecretLen && (redact.SensitiveKey(k) || looksRandom(v)) {
					bw.known = append(bw.known, v)
				}
			}
			sort.Strings(keys)
			secretList = append(secretList, map[string]any{"name": s.Name, "type": string(s.Type), "keys": keys})
		}
	}
	// Longest first, so a value that contains another is masked whole.
	sort.Slice(bw.known, func(i, j int) bool { return len(bw.known[i]) > len(bw.known[j]) })

	bw.add("README.md", []byte(readme(in)))
	bw.yaml("versions.yaml", in.Versions)
	bw.add("doctor.txt", doctorText(in.Findings))
	bw.json("doctor.json", in.Findings)
	bw.yaml("secrets.yaml", secretList)

	if rel := in.Current; rel != nil {
		bw.yaml("release/values.yaml", redact.Value(clone(rel.Config)))
		bw.add("release/manifest.yaml", []byte(manifest(rel.Manifest)))
		var hist []map[string]any
		for _, h := range in.History {
			hist = append(hist, map[string]any{
				"revision": h.Version, "status": h.Info.Status.String(), "chart": h.Chart.Metadata.Version,
				"appVersion": h.Chart.Metadata.AppVersion, "updated": h.Info.LastDeployed.Format(time.RFC3339),
				"description": h.Info.Description,
			})
		}
		bw.yaml("release/history.yaml", hist)
	}

	bw.resources(ctx, cs, ns)
	bw.nodes(ctx, cs)
	bw.logs(ctx, cs, ns, in.LogLines)

	if len(bw.summary.Errors) > 0 {
		bw.add("errors.txt", []byte(strings.Join(bw.summary.Errors, "\n")+"\n"))
	}
	if err := bw.tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return bw.summary, nil
}

func (b *writer) fail(what string, err error) {
	b.summary.Errors = append(b.summary.Errors, fmt.Sprintf("%s: %v", what, err))
}

// add writes one file. Whatever produced it, the file then goes through the
// text redaction and has every known Secret value masked.
func (b *writer) add(name string, data []byte) {
	data = []byte(redact.Text(string(data)))
	for _, v := range b.known {
		data = bytes.ReplaceAll(data, v, []byte(redact.Mask))
	}
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: b.now, Typeflag: tar.TypeReg}
	if err := b.tw.WriteHeader(hdr); err != nil {
		b.fail(name, err)
		return
	}
	if _, err := b.tw.Write(data); err != nil {
		b.fail(name, err)
		return
	}
	b.summary.Files++
}

func (b *writer) yaml(name string, v any) {
	out, err := yaml.Marshal(v)
	if err != nil {
		b.fail(name, err)
		return
	}
	b.add(name, out)
}

func (b *writer) json(name string, v any) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b.fail(name, err)
		return
	}
	b.add(name, append(out, '\n'))
}

// resources writes each kind as a redacted YAML list.
func (b *writer) resources(ctx context.Context, cs kubernetes.Interface, ns string) {
	opts := metav1.ListOptions{}
	kinds := []struct {
		file string
		list func() (any, error)
	}{
		{"deployments", func() (any, error) { return cs.AppsV1().Deployments(ns).List(ctx, opts) }},
		{"statefulsets", func() (any, error) { return cs.AppsV1().StatefulSets(ns).List(ctx, opts) }},
		{"daemonsets", func() (any, error) { return cs.AppsV1().DaemonSets(ns).List(ctx, opts) }},
		{"pods", func() (any, error) { return cs.CoreV1().Pods(ns).List(ctx, opts) }},
		{"services", func() (any, error) { return cs.CoreV1().Services(ns).List(ctx, opts) }},
		{"ingresses", func() (any, error) { return cs.NetworkingV1().Ingresses(ns).List(ctx, opts) }},
		{"persistentvolumeclaims", func() (any, error) { return cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, opts) }},
		{"jobs", func() (any, error) { return cs.BatchV1().Jobs(ns).List(ctx, opts) }},
		{"configmaps", func() (any, error) { return cs.CoreV1().ConfigMaps(ns).List(ctx, opts) }},
		{"events", func() (any, error) { return cs.CoreV1().Events(ns).List(ctx, opts) }},
	}
	for _, k := range kinds {
		list, err := k.list()
		if err != nil {
			b.fail(k.file, err)
			continue
		}
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(list)
		if err != nil {
			b.fail(k.file, err)
			continue
		}
		items, _ := obj["items"].([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				tidy(m)
			}
		}
		b.yaml("resources/"+k.file+".yaml", redact.Value(items))
	}
}

// tidy drops what only adds size: managed fields, the last-applied copy of
// the object, and ConfigMaps' binary data.
func tidy(m map[string]any) {
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	if _, ok := m["binaryData"]; ok {
		m["binaryData"] = "[omitted]"
	}
}

// nodes writes each node's capacity, conditions and taints. Node lists are
// cluster-scoped, so a namespace-only role cannot read them; that is noted,
// not fatal.
func (b *writer) nodes(ctx context.Context, cs kubernetes.Interface) {
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		b.fail("nodes", err)
		return
	}
	var out []map[string]any
	for _, n := range list.Items {
		conds := map[string]string{}
		for _, c := range n.Status.Conditions {
			conds[string(c.Type)] = string(c.Status)
		}
		out = append(out, map[string]any{
			"name": n.Name, "unschedulable": n.Spec.Unschedulable, "taints": n.Spec.Taints,
			"capacity": n.Status.Capacity, "allocatable": n.Status.Allocatable, "conditions": conds,
			"kubelet": n.Status.NodeInfo.KubeletVersion, "os": n.Status.NodeInfo.OSImage, "arch": n.Status.NodeInfo.Architecture,
			"instanceType": n.Labels["node.kubernetes.io/instance-type"], "zone": n.Labels["topology.kubernetes.io/zone"],
		})
	}
	b.yaml("nodes.yaml", out)
}

// logs writes the tail of every container's log, and of its previous run
// when it restarted.
func (b *writer) logs(ctx context.Context, cs kubernetes.Interface, ns string, lines int64) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		b.fail("logs", err)
		return
	}
	for _, p := range pods.Items {
		restarts := map[string]int32{}
		for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			restarts[s.Name] = s.RestartCount
		}
		for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
			b.log(ctx, cs, ns, p.Name, c.Name, false, lines)
			if restarts[c.Name] > 0 {
				b.log(ctx, cs, ns, p.Name, c.Name, true, lines)
			}
		}
	}
}

func (b *writer) log(ctx context.Context, cs kubernetes.Interface, ns, pod, container string, previous bool, lines int64) {
	name := fmt.Sprintf("logs/%s/%s.log", pod, container)
	if previous {
		name = fmt.Sprintf("logs/%s/%s.previous.log", pod, container)
	}
	rc, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &lines}).Stream(ctx)
	if err != nil {
		// A container that never started has no log; that is not worth an
		// error line per container.
		return
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	var out bytes.Buffer
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		out.WriteString(redact.Text(sc.Text()))
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		b.fail(name, err)
	}
	b.add(name, out.Bytes())
}

// manifest returns the release manifest with every Secret reduced to its key
// names and the rest redacted.
func manifest(m string) string {
	var docs []string
	for _, doc := range strings.Split(m, "\n---") {
		doc = strings.TrimSpace(strings.TrimPrefix(doc, "---"))
		if doc == "" {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || obj == nil {
			docs = append(docs, "# (a document that could not be parsed was left out)")
			continue
		}
		if obj["kind"] == "Secret" {
			for _, field := range []string{"data", "stringData"} {
				if d, ok := obj[field].(map[string]any); ok {
					for k := range d {
						d[k] = redact.Mask
					}
				}
			}
		}
		out, err := yaml.Marshal(redact.Value(obj))
		if err != nil {
			continue
		}
		docs = append(docs, strings.TrimSpace(string(out)))
	}
	return strings.Join(docs, "\n---\n") + "\n"
}

func clone(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func doctorText(rs []preflight.Result) []byte {
	var b bytes.Buffer
	if err := preflight.Print(&b, rs, false); err != nil {
		return []byte(err.Error())
	}
	return b.Bytes()
}

func readme(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# NeuralTrust support bundle\n\nNamespace %s, release %s, collected %s by ntctl.\n\n", in.Namespace, in.Release, in.Now.UTC().Format(time.RFC3339))
	b.WriteString(`## Contents

- versions.yaml: ntctl, Kubernetes and chart versions
- doctor.txt, doctor.json: the ntctl doctor report
- secrets.yaml: Secret names, types and key names (no values)
- release/: the release's values, manifest and history
- resources/: Deployments, StatefulSets, DaemonSets, Pods, Services, Ingresses, PersistentVolumeClaims, Jobs, ConfigMaps and Events in the namespace
- nodes.yaml: node capacity, conditions and taints
- logs/: the last lines of each container's log, and of its previous run when it restarted
- errors.txt: anything that could not be collected

## What was removed

`)
	for _, r := range redact.Rules {
		b.WriteString("- " + r + "\n")
	}
	b.WriteString("- Any text equal to a value stored in a Secret of the namespace, wherever it appears, when the value is under one of the key names above or looks random (20 characters or more, letters and digits, no dots, colons or slashes).\n")
	b.WriteString("\nRemoved values read " + redact.Mask + ". Check the files before sending the bundle.\n")
	return b.String()
}
