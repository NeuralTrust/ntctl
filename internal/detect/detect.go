// Package detect reads the facts about a cluster that an install depends on.
package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
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
	// GatewayAPI is true when the cluster serves gateway.networking.k8s.io/v1.
	GatewayAPI bool
	// Gateways in every namespace; GatewaysRead is false when they could not
	// be listed (no dynamic client, or no permission).
	Gateways     []gatewayapi.Gateway
	GatewaysRead bool
	Nodes        int
	// Allocatable on schedulable nodes: CPU in millicores, memory in bytes.
	CPUMilli int64
	Memory   int64
}

const (
	defaultIngressAnnotation = "ingressclass.kubernetes.io/is-default-class"
	defaultStorageAnnotation = "storageclass.kubernetes.io/is-default-class"
	betaStorageAnnotation    = "storageclass.beta.kubernetes.io/is-default-class"
)

// Inspect queries the cluster. It needs only read access. dyn may be nil; the
// Gateways are then left unread.
func Inspect(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface) (*Cluster, error) {
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
		if g.Name == gatewayapi.Group {
			for _, v := range g.Versions {
				c.GatewayAPI = c.GatewayAPI || v.Version == "v1"
			}
		}
	}
	if c.GatewayAPI && dyn != nil {
		// A Gateway list forbidden by RBAC leaves GatewaysRead false rather
		// than failing: the checks that need it say they could not look.
		if gws, err := gatewayapi.ListGateways(ctx, dyn); err == nil {
			c.Gateways, c.GatewaysRead = gws, true
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

// NamespaceLabels returns a namespace's labels, the ones a Gateway listener's
// allowedRoutes selector sees. A namespace that does not exist yet gets only
// the kubernetes.io/metadata.name label the API server adds on creation.
func NamespaceLabels(ctx context.Context, cs kubernetes.Interface, ns string) (map[string]string, error) {
	out := map[string]string{"kubernetes.io/metadata.name": ns}
	n, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	for k, v := range n.Labels {
		out[k] = v
	}
	return out, nil
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
