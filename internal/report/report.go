// Package report shows what an install looks like from the outside: workload
// readiness, endpoints, and the DNS records an operator has to create.
package report

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
)

// Workload is the readiness of one Deployment, StatefulSet or DaemonSet.
type Workload struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Ready   int32  `json:"ready"`
	Desired int32  `json:"desired"`
}

// Problem is a pod that needs attention.
type Problem struct {
	Pod      string `json:"pod"`
	Reason   string `json:"reason"`
	Restarts int32  `json:"restarts"`
}

// Record is a DNS record to create for an endpoint.
type Record struct {
	Host   string `json:"host"`
	Type   string `json:"type"` // A or CNAME; empty while the address is pending
	Target string `json:"target"`
}

// Status is the state of a namespace.
type Status struct {
	Workloads []Workload `json:"workloads"`
	Problems  []Problem  `json:"problems"`
	Records   []Record   `json:"dns"`
}

// Collect reads workloads, pods, ingresses and, when dyn is not nil, the
// HTTPRoutes in the namespace.
func Collect(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, ns string) (*Status, error) {
	s := &Status{}
	deps, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	for _, d := range deps.Items {
		s.Workloads = append(s.Workloads, Workload{"Deployment", d.Name, d.Status.ReadyReplicas, replicas(d.Spec.Replicas)})
	}
	sts, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	for _, d := range sts.Items {
		s.Workloads = append(s.Workloads, Workload{"StatefulSet", d.Name, d.Status.ReadyReplicas, replicas(d.Spec.Replicas)})
	}
	dss, err := cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	for _, d := range dss.Items {
		s.Workloads = append(s.Workloads, Workload{"DaemonSet", d.Name, d.Status.NumberReady, d.Status.DesiredNumberScheduled})
	}
	sort.Slice(s.Workloads, func(i, j int) bool { return s.Workloads[i].Name < s.Workloads[j].Name })

	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for _, p := range pods.Items {
		if pr, bad := problem(p); bad {
			s.Problems = append(s.Problems, pr)
		}
	}

	ings, err := cs.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingresses: %w", err)
	}
	seen := map[string]bool{}
	for _, ing := range ings.Items {
		typ, target := "", ""
		for _, lb := range ing.Status.LoadBalancer.Ingress {
			if lb.Hostname != "" {
				typ, target = "CNAME", lb.Hostname
				break
			}
			if lb.IP != "" {
				typ, target = "A", lb.IP
				break
			}
		}
		for _, rule := range ing.Spec.Rules {
			if rule.Host == "" || seen[rule.Host] {
				continue
			}
			seen[rule.Host] = true
			s.Records = append(s.Records, Record{Host: rule.Host, Type: typ, Target: target})
		}
	}
	if dyn != nil {
		if err := s.routeRecords(ctx, dyn, ns, seen); err != nil {
			return nil, err
		}
	}
	sort.Slice(s.Records, func(i, j int) bool { return s.Records[i].Host < s.Records[j].Host })
	return s, nil
}

// routeRecords adds a record per HTTPRoute hostname, pointing at the address
// of the route's first parent Gateway.
func (s *Status) routeRecords(ctx context.Context, dyn dynamic.Interface, ns string, seen map[string]bool) error {
	routes, err := gatewayapi.ListRoutes(ctx, dyn, ns)
	if err != nil {
		return err
	}
	gateways := map[string]*gatewayapi.Gateway{}
	for _, r := range routes {
		typ, target := "", ""
		if len(r.Parents) > 0 {
			p := r.Parents[0]
			key := p.Namespace + "/" + p.Name
			gw, cached := gateways[key]
			if !cached {
				// A Gateway that cannot be read leaves the address pending;
				// doctor says why.
				gw, _ = gatewayapi.GetGateway(ctx, dyn, p.Namespace, p.Name)
				gateways[key] = gw
			}
			if gw != nil {
				typ, target = gw.DNS()
			}
		}
		for _, h := range r.Hostnames {
			if seen[h] {
				continue
			}
			seen[h] = true
			s.Records = append(s.Records, Record{Host: h, Type: typ, Target: target})
		}
	}
	return nil
}

// Ready reports whether every workload is fully ready.
func (s *Status) Ready() bool {
	for _, w := range s.Workloads {
		if w.Ready < w.Desired {
			return false
		}
	}
	return true
}

// Print writes a human-readable view.
func (s *Status) Print(w io.Writer) {
	ready := 0
	for _, wl := range s.Workloads {
		if wl.Ready >= wl.Desired {
			ready++
		}
	}
	fmt.Fprintf(w, "Workloads: %d/%d ready\n", ready, len(s.Workloads))
	for _, wl := range s.Workloads {
		mark := "ok  "
		if wl.Ready < wl.Desired {
			mark = "WAIT"
		}
		fmt.Fprintf(w, "  %s %-45s %d/%d\n", mark, wl.Name, wl.Ready, wl.Desired)
	}
	if len(s.Problems) > 0 {
		fmt.Fprintln(w, "\nPods that need attention:")
		for _, p := range s.Problems {
			fmt.Fprintf(w, "  %-55s %s (restarts: %d)\n", p.Pod, p.Reason, p.Restarts)
		}
	}
	s.PrintDNS(w)
}

// PrintDNS writes the DNS records to create.
func (s *Status) PrintDNS(w io.Writer) {
	if len(s.Records) == 0 {
		return
	}
	fmt.Fprintln(w, "\nDNS records to create:")
	pending := false
	for _, r := range s.Records {
		if r.Type == "" {
			pending = true
			fmt.Fprintf(w, "  %-45s (address pending)\n", r.Host)
			continue
		}
		fmt.Fprintf(w, "  %-45s %-5s %s\n", r.Host, r.Type, r.Target)
	}
	if pending {
		fmt.Fprintln(w, "  The load balancer has not published an address yet; run `ntctl status` again in a minute.")
	}
}

func problem(p corev1.Pod) (Problem, bool) {
	var restarts int32
	reason := ""
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		restarts += cs.RestartCount
		if cs.State.Waiting != nil {
			switch r := cs.State.Waiting.Reason; r {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "InvalidImageName":
				reason = r
			}
		}
	}
	if reason == "" && p.Status.Phase == corev1.PodPending {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				reason = "Unschedulable: " + schedulerReason(c.Message)
			}
		}
	}
	if reason == "" && p.Status.Phase == corev1.PodFailed {
		reason = "Failed"
	}
	if reason == "" && restarts < 5 {
		return Problem{}, false
	}
	if reason == "" {
		reason = "restarting"
	}
	return Problem{Pod: p.Name, Reason: reason, Restarts: restarts}, true
}

// schedulerReason keeps the cause from a scheduler message such as
// "0/3 nodes are available: 3 Insufficient memory. preemption: ...".
func schedulerReason(msg string) string {
	msg = strings.TrimSpace(msg)
	if _, rest, ok := strings.Cut(msg, "nodes are available: "); ok {
		msg = rest
	}
	if cause, _, ok := strings.Cut(msg, "."); ok {
		msg = cause
	}
	// "1 Insufficient memory" reads better without the node count.
	if n, rest, ok := strings.Cut(msg, " "); ok && strings.Trim(n, "0123456789") == "" {
		msg = rest
	}
	return msg
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}
