// Package detect reads the facts about a cluster that an install depends on.
package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Cluster describes what an install can rely on.
type Cluster struct {
	Version             string
	Platform            string // openshift, aws, gcp, azure or kubernetes
	IngressClasses      []string
	DefaultIngressClass string
	StorageClasses      []string
	DefaultStorageClass string
	HasRoutes           bool
	Nodes               int
	// Allocatable on schedulable nodes: CPU in millicores, memory in bytes.
	CPUMilli int64
	Memory   int64
}

const (
	defaultIngressAnnotation = "ingressclass.kubernetes.io/is-default-class"
	defaultStorageAnnotation = "storageclass.kubernetes.io/is-default-class"
	betaStorageAnnotation    = "storageclass.beta.kubernetes.io/is-default-class"
)

// Inspect queries the cluster. It needs only read access.
func Inspect(ctx context.Context, cs kubernetes.Interface) (*Cluster, error) {
	c := &Cluster{}
	v, err := cs.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("reaching the cluster: %w", err)
	}
	c.Version = v.GitVersion

	groups, err := cs.Discovery().ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("listing API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == "route.openshift.io" {
			c.HasRoutes = true
		}
	}

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !schedulable(n) {
			continue
		}
		c.Nodes++
		c.CPUMilli += n.Status.Allocatable.Cpu().MilliValue()
		c.Memory += n.Status.Allocatable.Memory().Value()
	}
	c.Platform = platform(c.HasRoutes, nodes.Items)

	ics, err := cs.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingress classes: %w", err)
	}
	for _, ic := range ics.Items {
		c.IngressClasses = append(c.IngressClasses, ic.Name)
		if ic.Annotations[defaultIngressAnnotation] == "true" {
			c.DefaultIngressClass = ic.Name
		}
	}

	scs, err := cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing storage classes: %w", err)
	}
	for _, sc := range scs.Items {
		c.StorageClasses = append(c.StorageClasses, sc.Name)
		if sc.Annotations[defaultStorageAnnotation] == "true" || sc.Annotations[betaStorageAnnotation] == "true" {
			c.DefaultStorageClass = sc.Name
		}
	}
	sort.Strings(c.IngressClasses)
	sort.Strings(c.StorageClasses)
	return c, nil
}

// platform maps the cluster to the chart's global.platform values.
func platform(hasRoutes bool, nodes []corev1.Node) string {
	if hasRoutes {
		return "openshift"
	}
	for _, n := range nodes {
		switch id := n.Spec.ProviderID; {
		case strings.HasPrefix(id, "aws://"):
			return "aws"
		case strings.HasPrefix(id, "gce://"):
			return "gcp"
		case strings.HasPrefix(id, "azure://"):
			return "azure"
		}
	}
	return "kubernetes"
}

// schedulable leaves out cordoned and control-plane nodes. Other taints (GPU
// pools, for example) are kept: workloads that need those nodes tolerate them.
func schedulable(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, t := range n.Spec.Taints {
		if t.Key == "node-role.kubernetes.io/control-plane" || t.Key == "node-role.kubernetes.io/master" {
			return false
		}
	}
	return true
}
