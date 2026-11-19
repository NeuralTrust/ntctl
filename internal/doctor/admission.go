package doctor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NeuralTrust/ntctl/internal/preflight"
)

// Admission refusals a controller reports as FailedCreate events: the pod is
// never created, so nothing about it shows up among the pods.
const (
	sccRefusal = "unable to validate against any security context constraint"
	psaRefusal = "violates PodSecurity"
)

// refusedObject is a controller whose pods admission refuses.
type refusedObject struct {
	kind, name, message string
}

// admissionFindings reports Jobs, ReplicaSets, StatefulSets and DaemonSets
// whose pods admission refuses: an OpenShift SecurityContextConstraint or the
// namespace's Pod Security level. Such a pod never exists, so the pod checks
// cannot see it, and an install waits for it until its timeout. An object
// that has created its pods since is skipped, as its events outlive the
// refusal.
func admissionFindings(ctx context.Context, in Input) []preflight.Result {
	evs, err := in.Clientset.CoreV1().Events(in.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedCreate"})
	if err != nil {
		return nil
	}
	latest := map[string]refusedObject{}
	for _, e := range evs.Items {
		if e.Type != corev1.EventTypeWarning || e.Reason != "FailedCreate" {
			continue
		}
		if !strings.Contains(e.Message, sccRefusal) && !strings.Contains(e.Message, psaRefusal) {
			continue
		}
		switch e.InvolvedObject.Kind {
		case "Job", "ReplicaSet", "StatefulSet", "DaemonSet":
		default:
			continue
		}
		latest[e.InvolvedObject.Kind+"/"+e.InvolvedObject.Name] = refusedObject{kind: e.InvolvedObject.Kind, name: e.InvolvedObject.Name, message: e.Message}
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []preflight.Result
	for _, k := range keys {
		o := latest[k]
		sa, still := podTemplateAccount(ctx, in, o.kind, o.name)
		if !still {
			continue
		}
		out = append(out, refusal(in.Namespace, o, sa))
	}
	return out
}

// refusal is the NT-DR-016 finding for one refused object.
func refusal(ns string, o refusedObject, sa string) preflight.Result {
	r := preflight.Result{
		Code: "NT-DR-016", Check: "admission", Severity: preflight.Fail,
		Message: fmt.Sprintf("%s %s cannot create its pods: admission refuses them (service account %s)", strings.ToLower(o.kind), o.name, sa),
	}
	if strings.Contains(o.message, sccRefusal) {
		r.Detail = []string{clip(sccDetail(o.message), 300)}
		r.Fix = "OpenShift's SecurityContextConstraints refuse the pod's security settings, usually a fixed runAsUser outside the namespace's UID range under restricted-v2. " +
			"Upgrade to a chart that leaves the UID to OpenShift on platform openshift. Until then, allow the service account a constraint that admits it: " +
			"`oc adm policy add-scc-to-user nonroot-v2 -z " + sa + " -n " + ns + "`; the controller retries on its own."
		return r
	}
	r.Detail = []string{clip(o.message, 300)}
	r.Fix = "The namespace's Pod Security level (label pod-security.kubernetes.io/enforce) refuses the pod. Upgrade to a chart whose pods meet it, or lower the namespace's level if your policy allows."
	return r
}

// sccDetail keeps the part of an SCC refusal that says why restricted-v2, the
// default constraint, refused the pod; the rest lists constraints the service
// account may not use.
func sccDetail(msg string) string {
	if i := strings.Index(msg, "restricted-v2"); i >= 0 {
		return msg[i:]
	}
	return msg
}

// podTemplateAccount returns the service account of a controller's pod
// template, and whether the controller still lacks the pods it wants. A
// controller that is gone, or has its pods by now, is no longer refused.
func podTemplateAccount(ctx context.Context, in Input, kind, name string) (string, bool) {
	var spec corev1.PodSpec
	switch kind {
	case "Job":
		j, err := in.Clientset.BatchV1().Jobs(in.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || j.Status.Active+j.Status.Succeeded+j.Status.Failed > 0 {
			return "", false
		}
		spec = j.Spec.Template.Spec
	case "ReplicaSet":
		rs, err := in.Clientset.AppsV1().ReplicaSets(in.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || rs.Spec.Replicas == nil || rs.Status.Replicas >= *rs.Spec.Replicas {
			return "", false
		}
		spec = rs.Spec.Template.Spec
	case "StatefulSet":
		s, err := in.Clientset.AppsV1().StatefulSets(in.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || s.Spec.Replicas == nil || s.Status.Replicas >= *s.Spec.Replicas {
			return "", false
		}
		spec = s.Spec.Template.Spec
	case "DaemonSet":
		d, err := in.Clientset.AppsV1().DaemonSets(in.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || d.Status.CurrentNumberScheduled >= d.Status.DesiredNumberScheduled {
			return "", false
		}
		spec = d.Spec.Template.Spec
	default:
		return "", false
	}
	if spec.ServiceAccountName == "" {
		return "default", true
	}
	return spec.ServiceAccountName, true
}
