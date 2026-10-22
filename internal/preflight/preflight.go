// Package preflight checks a cluster against what a rendered release needs,
// before anything is applied.
package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/manifest"
)

// Severity of a check result.
type Severity string

// Severities, in increasing order.
const (
	OK   Severity = "OK"
	Warn Severity = "WARN"
	Fail Severity = "FAIL"
)

// Result is one check outcome. Fix says what to do when it is not OK.
type Result struct {
	Code     string   `json:"code"`
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Fix      string   `json:"fix,omitempty"`
	// Detail holds supporting lines, such as a crashing container's last log
	// lines.
	Detail []string `json:"detail,omitempty"`
}

// Mode says which operation the checks are for.
type Mode string

// Modes.
const (
	ForInstall Mode = "install"
	ForUpgrade Mode = "upgrade"
)

// SecretLookup returns the keys of a Secret in the release namespace, and
// whether it exists.
type SecretLookup func(ctx context.Context, name string) (keys []string, exists bool, err error)

// Input is everything the checks look at.
type Input struct {
	Mode          Mode
	Release       string
	Namespace     string
	ReleaseExists bool
	Cluster       *detect.Cluster
	Needs         manifest.Needs
	// Platform is the effective global.platform value.
	Platform string
	// IngressProvider is the effective global.ingress.provider value.
	IngressProvider string
	// NamespaceLabels are the release namespace's labels, which a Gateway
	// listener's allowedRoutes selector is matched against.
	NamespaceLabels map[string]string
	// WillCreate lists Secrets ntctl creates during install, with their keys.
	WillCreate map[string][]string
	Secrets    SecretLookup
	// IgnoreCapacity turns a capacity failure into a warning, for clusters
	// meant to run short (a test cluster, or capacity added out of band).
	IgnoreCapacity bool
	// DeploymentMode is the effective global.deploymentMode: only a hybrid
	// data plane dials a control plane outside the cluster.
	DeploymentMode string
	// Resolve looks a host name up from where ntctl runs; nil skips the
	// control-plane check.
	Resolve func(ctx context.Context, host string) ([]string, error)
}

// Run executes every check.
func Run(ctx context.Context, in Input) ([]Result, error) {
	rs := []Result{
		release(in),
		platform(in),
		storage(in),
		ingress(in),
	}
	secretResults, err := secrets(ctx, in)
	if err != nil {
		return nil, err
	}
	rs = append(rs, secretResults...)
	rs = append(rs, capacity(in))
	if r, ok := exposure(in); ok {
		rs = append(rs, r)
	}
	if r, ok := controlPlane(ctx, in); ok {
		rs = append(rs, r)
	}
	return rs, nil
}

// vpnRange holds the shared address space (RFC 6598) that VPN clients such as
// Twingate and Tailscale hand out for the names they serve. A name that
// resolves only into it here resolves for this machine alone, never for pods.
var vpnRange = netip.MustParsePrefix("100.64.0.0/10")

// controlPlane warns when the control plane a hybrid data plane dials does
// not resolve here, or resolves only to a VPN client's address. It only
// warns: the cluster may use DNS this machine does not (split DNS).
func controlPlane(ctx context.Context, in Input) (Result, bool) {
	if in.Resolve == nil || in.DeploymentMode != "hybrid" || len(in.Needs.Dials) == 0 {
		return Result{}, false
	}
	var hosts []string
	seen := map[string]bool{}
	for _, addr := range in.Needs.Dials {
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		if !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	r := Result{Code: "NT-PF-010", Check: "dial", Severity: OK,
		Message: "the control plane resolves: " + strings.Join(hosts, ", ")}
	for _, h := range hosts {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := in.Resolve(lctx, h)
		cancel()
		switch {
		case err != nil || len(addrs) == 0:
			r.Detail = append(r.Detail, h+" does not resolve from this machine")
		case onlyVPN(addrs):
			r.Detail = append(r.Detail, fmt.Sprintf("%s resolves here only to %s, an address a VPN client such as Twingate or Tailscale hands out", h, strings.Join(addrs, ", ")))
		}
	}
	if len(r.Detail) == 0 {
		return r, true
	}
	r.Severity = Warn
	r.Message = fmt.Sprintf("the data plane may not reach its control plane: %d of %d names resolve only here or not at all", len(r.Detail), len(hosts))
	r.Fix = "The data plane dials these names from inside the cluster, so the cluster's DNS must resolve them to addresses its pods can reach. " +
		"Publish them in DNS the cluster uses, or point the data plane at a control plane the cluster reaches (--control-plane). " +
		"If the cluster resolves them differently from this machine, ignore this."
	return r, true
}

// onlyVPN reports whether every address is in the VPN range.
func onlyVPN(addrs []string) bool {
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil || !vpnRange.Contains(ip.Unmap()) {
			return false
		}
	}
	return true
}

// internalLB are, per platform, the Service annotations that keep a
// LoadBalancer's address private, with the value that does.
var internalLB = map[string]map[string]string{
	"aws":   {"service.beta.kubernetes.io/aws-load-balancer-scheme": "internal", "service.beta.kubernetes.io/aws-load-balancer-internal": "true"},
	"azure": {"service.beta.kubernetes.io/azure-load-balancer-internal": "true"},
	"gcp":   {"networking.gke.io/load-balancer-type": "Internal", "cloud.google.com/load-balancer-type": "Internal"},
}

// exposure warns about LoadBalancer Services the cloud will give a public
// address, on the clouds whose annotations say so. A central control plane
// renders them for DataBridge and config sync; data planes that reach it
// over the internet need exactly that, and private ones do not.
func exposure(in Input) (Result, bool) {
	known, ok := internalLB[strings.ToLower(in.Platform)]
	if !ok || len(in.Needs.LoadBalancers) == 0 {
		return Result{}, false
	}
	var public []string
	for _, lb := range in.Needs.LoadBalancers {
		private := false
		for k, want := range known {
			if strings.EqualFold(strings.TrimSpace(lb.Annotations[k]), want) {
				private = true
			}
		}
		if !private {
			public = append(public, lb.Name)
		}
	}
	r := Result{Code: "NT-PF-009", Check: "exposure", Severity: OK,
		Message: fmt.Sprintf("%d LoadBalancer Services get private addresses", len(in.Needs.LoadBalancers))}
	if len(public) == 0 {
		return r, true
	}
	sort.Strings(public)
	r.Severity = Warn
	r.Message = fmt.Sprintf("%d LoadBalancer Services get a public address on %s: %s", len(public), in.Platform, strings.Join(public, ", "))
	r.Fix = "Expected when data planes reach this install over the internet. If they reach it privately, keep the address private: on AWS set global.controlPlane.loadBalancerScheme: internal; " +
		"on Azure annotate the Services with service.beta.kubernetes.io/azure-load-balancer-internal: \"true\", on GKE with networking.gke.io/load-balancer-type: Internal, in your values."
	return r, true
}

// Failed reports whether any result is a failure.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Severity == Fail {
			return true
		}
	}
	return false
}

func release(in Input) Result {
	r := Result{Code: "NT-PF-002", Check: "release", Severity: OK}
	switch {
	case in.Mode == ForInstall && in.ReleaseExists:
		r.Severity = Fail
		r.Message = fmt.Sprintf("release %q already exists in namespace %q", in.Release, in.Namespace)
		r.Fix = "`ntctl install` adds products to an existing install; use `ntctl upgrade` to change anything else, or `ntctl preflight --for upgrade` to check it."
	case in.Mode == ForUpgrade && !in.ReleaseExists:
		r.Severity = Fail
		r.Message = fmt.Sprintf("no release %q in namespace %q", in.Release, in.Namespace)
		r.Fix = "Use `ntctl install` for a first install, or pass --namespace/--release."
	case in.ReleaseExists:
		r.Message = fmt.Sprintf("release %q found", in.Release)
	default:
		r.Message = fmt.Sprintf("no existing release %q in namespace %q", in.Release, in.Namespace)
	}
	return r
}

func platform(in Input) Result {
	r := Result{Code: "NT-PF-003", Check: "platform", Severity: OK,
		Message: fmt.Sprintf("Kubernetes %s, %d schedulable nodes, detected platform %q", in.Cluster.Version, in.Cluster.Nodes, in.Cluster.Platform)}
	if in.Platform != "" && in.Platform != in.Cluster.Platform && in.Cluster.Platform != "kubernetes" {
		r.Severity = Warn
		r.Message = fmt.Sprintf("global.platform is %q but the cluster looks like %q", in.Platform, in.Cluster.Platform)
		r.Fix = fmt.Sprintf("Pass --platform %s, or keep %q if that is deliberate.", in.Cluster.Platform, in.Platform)
	}
	return r
}

func storage(in Input) Result {
	r := Result{Code: "NT-PF-004", Check: "storage", Severity: OK}
	var missing []string
	for _, sc := range in.Needs.StorageClasses {
		if !has(in.Cluster.StorageClasses, sc) {
			missing = append(missing, sc)
		}
	}
	switch {
	case len(missing) > 0:
		r.Severity = Fail
		r.Message = "storage classes requested by the values do not exist: " + strings.Join(missing, ", ")
		r.Fix = "Set the storageClass values to one of: " + listOrNone(in.Cluster.StorageClasses) + "."
	case in.Needs.DefaultStorage && in.Cluster.DefaultStorageClass == "":
		r.Severity = Fail
		r.Message = "the release creates volumes without a storage class, and the cluster has no default StorageClass"
		r.Fix = "Mark a StorageClass as default, or set the storageClass values. Available: " + listOrNone(in.Cluster.StorageClasses) + "."
	case in.Needs.DefaultStorage || len(in.Needs.StorageClasses) > 0:
		r.Message = "volumes can be provisioned"
		if in.Cluster.DefaultStorageClass != "" {
			r.Message += fmt.Sprintf(" (default StorageClass %q)", in.Cluster.DefaultStorageClass)
		}
	default:
		r.Message = "the release creates no volumes"
	}
	return r
}

func ingress(in Input) Result {
	r := Result{Code: "NT-PF-005", Check: "ingress", Severity: OK}
	if in.Needs.Routes && !in.Cluster.HasRoutes {
		r.Severity = Fail
		r.Message = "the values render OpenShift Routes, but this cluster has no route.openshift.io API"
		r.Fix = "Use --platform with the cluster's real platform, or set the ingress resourceType to ingress."
		return r
	}
	if in.Needs.HTTPRoutes {
		return gatewayRoutes(in)
	}
	if in.IngressProvider == "gateway-api" && (in.Needs.DefaultIngress || len(in.Needs.IngressClasses) > 0) {
		r.Severity = Fail
		r.Message = "global.ingress.provider is gateway-api, but this chart version renders Ingresses: it predates Gateway API support"
		r.Fix = "Install a chart version with Gateway API support (--version), or drop global.ingress.provider."
		return r
	}
	var missing []string
	for _, ic := range in.Needs.IngressClasses {
		if !has(in.Cluster.IngressClasses, ic) {
			missing = append(missing, ic)
		}
	}
	switch {
	case len(missing) > 0:
		r.Severity = Warn
		r.Message = "ingress classes requested by the values do not exist: " + strings.Join(missing, ", ")
		r.Fix = "Install that ingress controller, or set the className values to one of: " + listOrNone(in.Cluster.IngressClasses) + "."
	case in.Needs.DefaultIngress && len(in.Cluster.IngressClasses) == 0 && in.Cluster.Platform == "gcp":
		// GKE's built-in controller serves Ingresses without an IngressClass.
		r.Message = "Ingresses will use GKE's built-in load balancer controller"
	case in.Needs.DefaultIngress && in.Cluster.DefaultIngressClass == "" && len(in.Cluster.IngressClasses) == 0:
		r.Severity = Warn
		r.Message = "the release creates Ingresses, but the cluster has no ingress controller"
		r.Fix = "Install an ingress controller (for example ingress-nginx) so the endpoints get an address."
		if len(in.Cluster.Gateways) > 0 {
			r.Fix = "Expose the endpoints through a Gateway the cluster already has (--gateway " + in.Cluster.Gateways[0].Ref() + "; available: " + gatewayList(in.Cluster.Gateways) + "), or install an ingress controller."
		}
	case in.Needs.DefaultIngress && in.Cluster.DefaultIngressClass == "":
		r.Severity = Warn
		r.Message = "Ingresses have no class and the cluster has no default IngressClass, so no controller may pick them up"
		r.Fix = "Mark one IngressClass as default, or set the className values to one of: " + listOrNone(in.Cluster.IngressClasses) + "."
	case in.Needs.Routes:
		r.Message = "OpenShift Routes available"
	default:
		r.Message = "ingress controller available"
	}
	return r
}

// gatewayRoutes checks that the Gateways the release's HTTPRoutes name exist
// and let routes from the release namespace attach.
func gatewayRoutes(in Input) Result {
	r := Result{Code: "NT-PF-005", Check: "ingress", Severity: OK}
	if !in.Cluster.GatewayAPI {
		r.Severity = Fail
		r.Message = "the values render Gateway API HTTPRoutes, but this cluster does not serve " + gatewayapi.Group + "/v1"
		r.Fix = "Install the Gateway API CRDs and an implementation with a Gateway, or drop global.ingress.provider=gateway-api."
		return r
	}
	if !in.Cluster.GatewaysRead {
		r.Severity = Warn
		r.Message = "could not list Gateways, so the HTTPRoutes' parents were not checked"
		r.Fix = "Grant get/list on gateways." + gatewayapi.Group + ", or check by hand that the parent Gateways exist and admit namespace " + in.Namespace + "."
		return r
	}
	labels := in.NamespaceLabels
	var attached []string
	for _, p := range in.Needs.Gateways {
		ref := p.In(in.Namespace)
		gw := findGateway(in.Cluster.Gateways, ref)
		if gw == nil {
			r.Severity = Fail
			r.Detail = append(r.Detail, fmt.Sprintf("Gateway %s/%s does not exist", ref.Namespace, ref.Name))
			continue
		}
		var listeners []gatewayapi.Listener
		if ref.SectionName != "" {
			l, ok := gw.Listener(ref.SectionName)
			if !ok {
				r.Severity = worse(r.Severity, Warn)
				r.Detail = append(r.Detail, fmt.Sprintf("Gateway %s has no listener %q (it has: %s)", gw.Ref(), ref.SectionName, listenerNames(gw)))
				continue
			}
			listeners = []gatewayapi.Listener{l}
		} else {
			listeners = gw.Listeners
		}
		admitted := false
		for _, l := range listeners {
			admitted = admitted || l.Admits(gw.Namespace, in.Namespace, labels)
		}
		if !admitted {
			r.Severity = worse(r.Severity, Warn)
			r.Detail = append(r.Detail, fmt.Sprintf("Gateway %s does not admit routes from namespace %s (allowedRoutes)", ref, in.Namespace))
			continue
		}
		desc := ref.String()
		if _, target := gw.DNS(); target != "" {
			desc += " (" + target + ")"
		}
		attached = append(attached, desc)
	}
	switch r.Severity {
	case Fail:
		r.Message = "HTTPRoutes name a Gateway that does not exist"
		r.Fix = "Set global.ingress.gatewayApi.parentRefs (or --gateway) to an existing Gateway. Available: " + gatewayList(in.Cluster.Gateways) + "."
	case Warn:
		r.Message = "HTTPRoutes may not attach to their Gateway"
		r.Fix = "Point the parentRefs at a listener that admits namespace " + in.Namespace + ", or widen that listener's allowedRoutes."
	default:
		r.Message = "HTTPRoutes attach to Gateway " + strings.Join(attached, ", ")
	}
	return r
}

func findGateway(gws []gatewayapi.Gateway, ref gatewayapi.ParentRef) *gatewayapi.Gateway {
	for i := range gws {
		if gws[i].Namespace == ref.Namespace && gws[i].Name == ref.Name {
			return &gws[i]
		}
	}
	return nil
}

func listenerNames(g *gatewayapi.Gateway) string {
	var names []string
	for _, l := range g.Listeners {
		names = append(names, l.Name)
	}
	return listOrNone(names)
}

func gatewayList(gws []gatewayapi.Gateway) string {
	var refs []string
	for _, g := range gws {
		refs = append(refs, g.Ref())
	}
	return listOrNone(refs)
}

func worse(a, b Severity) Severity {
	if a == Fail || b == Fail {
		return Fail
	}
	if a == Warn || b == Warn {
		return Warn
	}
	return OK
}

func secrets(ctx context.Context, in Input) ([]Result, error) {
	type want struct {
		keys []string
		by   string
	}
	wanted := map[string]*want{}
	for _, ref := range in.Needs.Secrets {
		w := wanted[ref.Name]
		if w == nil {
			w = &want{by: ref.By}
			wanted[ref.Name] = w
		}
		if ref.Key != "" {
			w.keys = append(w.keys, ref.Key)
		}
	}
	for _, name := range in.Needs.PullSecrets {
		if wanted[name] == nil {
			wanted[name] = &want{by: "imagePullSecrets"}
		}
	}
	names := make([]string, 0, len(wanted))
	for n := range wanted {
		names = append(names, n)
	}
	sort.Strings(names)

	var missing, missingKeys []string
	planned := 0
	for _, name := range names {
		w := wanted[name]
		keys, exists, err := in.Secrets(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("reading Secret %s: %w", name, err)
		}
		if !exists {
			if pk, ok := in.WillCreate[name]; ok {
				keys, exists = pk, true
				planned++
			}
		}
		if !exists {
			missing = append(missing, fmt.Sprintf("%s (used by %s)", name, w.by))
			continue
		}
		for _, k := range w.keys {
			if !has(keys, k) {
				missingKeys = append(missingKeys, name+"/"+k)
			}
		}
	}
	msg := fmt.Sprintf("all %d Secrets the release reads are present", len(names))
	if planned > 0 {
		msg = fmt.Sprintf("%d of %d Secrets the release reads are present; ntctl %s creates the other %d", len(names)-planned, len(names), in.Mode, planned)
	}
	r := Result{Code: "NT-PF-006", Check: "secrets", Severity: OK, Message: msg}
	if len(missing) > 0 || len(missingKeys) > 0 {
		r.Severity = Fail
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "missing Secrets: "+strings.Join(missing, ", "))
		}
		if len(missingKeys) > 0 {
			parts = append(parts, "missing keys: "+strings.Join(missingKeys, ", "))
		}
		r.Message = strings.Join(parts, "; ")
		r.Fix = "`ntctl install` creates the registry and token Secrets from --registry-key and your console tokens, and the first admin's (onprem-superadmin) in external and saas installs. Create any other Secret your values reference in namespace " + in.Namespace + "."
		if in.Mode == ForUpgrade {
			r.Fix = "`ntctl upgrade --products <product>` creates the token Secrets of a product it adds, from your console tokens. Create any other Secret your values reference in namespace " + in.Namespace + "."
		}
	}
	return []Result{r}, nil
}

// capacityWarn is the share of free capacity above which requests are too
// tight to absorb a rolling upgrade's extra pod (as in the chart's sizing guide).
const capacityWarn = 0.85

func capacity(in Input) Result {
	if len(in.Cluster.NodeList) == 0 || len(in.Needs.Groups) == 0 {
		return totalCapacity(in)
	}
	r := Result{Code: "NT-PF-008", Check: "capacity", Severity: OK}
	free := map[string]detect.Usage{}
	for _, n := range in.Cluster.NodeList {
		u := detect.Usage{CPUMilli: n.CPUMilli, Memory: n.Memory}
		for ns, used := range n.Used {
			// On an upgrade the release's own pods are already running; they
			// are in the requests below, so they must not count twice.
			if ns != in.Namespace {
				u.CPUMilli -= used.CPUMilli
				u.Memory -= used.Memory
			}
		}
		free[n.Name] = u
	}

	// Each group can use only the nodes whose taints it tolerates and whose
	// labels match its nodeSelector. Groups with the same nodes share a pool.
	// Pools whose nodes overlap without being equal each count the shared
	// nodes, so the check is optimistic there; the chart's shapes keep them
	// apart (general workloads on untainted nodes, GPU workers on the GPU pool).
	type pool struct {
		nodes      []string
		label      string
		need, room detect.Usage
		maxPod     detect.Usage
		// bigFree is the most room free on one node now; bigAlloc the most
		// one node has in total, which bounds any pod whatever else runs.
		bigFree  detect.Usage
		bigAlloc detect.Usage
	}
	pools := map[string]*pool{}
	var order []string
	usable := map[string]bool{}
	for _, g := range in.Needs.Groups {
		var nodes []string
		for _, n := range in.Cluster.NodeList {
			if tolerates(g.Tolerations, n.Taints) && selects(g.NodeSelector, n.Labels) {
				nodes = append(nodes, n.Name)
				usable[n.Name] = true
			}
		}
		if len(nodes) == 0 {
			r.Severity = Fail
			r.Message = fmt.Sprintf("no node can run %s: none matches its nodeSelector %v and tolerates the node taints", describeGroup(g), g.NodeSelector)
			r.Fix = "Add nodes with the labels and taints these workloads expect (for GPU workers, a GPU pool), or change their nodeSelector and tolerations in your values."
			return r
		}
		key := strings.Join(nodes, ",")
		p, ok := pools[key]
		if !ok {
			p = &pool{nodes: nodes, label: describeGroup(g)}
			for _, name := range nodes {
				f := free[name]
				p.room.CPUMilli += f.CPUMilli
				p.room.Memory += f.Memory
				p.bigFree.CPUMilli = max(p.bigFree.CPUMilli, f.CPUMilli)
				p.bigFree.Memory = max(p.bigFree.Memory, f.Memory)
			}
			for _, n := range in.Cluster.NodeList {
				if slices.Contains(nodes, n.Name) {
					p.bigAlloc.CPUMilli = max(p.bigAlloc.CPUMilli, n.CPUMilli)
					p.bigAlloc.Memory = max(p.bigAlloc.Memory, n.Memory)
				}
			}
			pools[key] = p
			order = append(order, key)
		} else {
			p.label = "workloads on the same nodes"
		}
		p.need.CPUMilli += g.CPUMilli
		p.need.Memory += g.Memory
		p.maxPod.CPUMilli = max(p.maxPod.CPUMilli, g.MaxPodCPUMilli)
		p.maxPod.Memory = max(p.maxPod.Memory, g.MaxPodMemory)
	}

	worst, binding := 0.0, ""
	for _, key := range order {
		p := pools[key]
		cpu, mem := ratio(p.need.CPUMilli, p.room.CPUMilli), ratio(p.need.Memory, p.room.Memory)
		r.Detail = append(r.Detail, fmt.Sprintf("%s, on %s: needs %.1f of %.1f vCPU free (%.0f%%) and %.1f of %.1f GiB free (%.0f%%)",
			p.label, plural(len(p.nodes), "node"), cores(p.need.CPUMilli), cores(p.room.CPUMilli), cpu*100,
			gib(p.need.Memory), gib(p.room.Memory), mem*100))
		if cpu > worst {
			worst, binding = cpu, fmt.Sprintf("CPU for %s needs %.0f%% of the %.1f vCPU free on its %s", p.label, cpu*100, cores(p.room.CPUMilli), plural(len(p.nodes), "node"))
		}
		if mem > worst {
			worst, binding = mem, fmt.Sprintf("memory for %s needs %.0f%% of the %.1f GiB free on its %s", p.label, mem*100, gib(p.room.Memory), plural(len(p.nodes), "node"))
		}
		// Larger than any node in total: no node, however empty, can take it.
		if p.maxPod.CPUMilli > p.bigAlloc.CPUMilli || p.maxPod.Memory > p.bigAlloc.Memory {
			r.Severity = Fail
			r.Message = fmt.Sprintf("the largest pod of %s (%.1f vCPU, %.1f GiB) is bigger than any of its nodes (at most %.1f vCPU, %.1f GiB allocatable)",
				p.label, cores(p.maxPod.CPUMilli), gib(p.maxPod.Memory), cores(p.bigAlloc.CPUMilli), gib(p.bigAlloc.Memory))
			r.Fix = "Use larger nodes for this pool, or lower that workload's requests in your values."
			return r
		}
		// Larger than any node's free room now: a shortfall like any other.
		if p.maxPod.CPUMilli > p.bigFree.CPUMilli || p.maxPod.Memory > p.bigFree.Memory {
			if worst <= 1 {
				worst = 1.01
				binding = fmt.Sprintf("the largest pod of %s (%.1f vCPU, %.1f GiB) fits no node's free room (at most %.1f vCPU, %.1f GiB)",
					p.label, cores(p.maxPod.CPUMilli), gib(p.maxPod.Memory), cores(p.bigFree.CPUMilli), gib(p.bigFree.Memory))
			}
		}
	}

	left := len(in.Cluster.NodeList) - len(usable)
	note := ""
	if left > 0 {
		note = fmt.Sprintf("; %s left out because no workload of the release can run there (taints or labels)", plural(left, "node"))
	}
	if !in.Cluster.UsageRead {
		note += "; pods already running could not be read, so their requests are not subtracted"
	}
	switch {
	case worst > 1 && (in.Cluster.Autoscaling || in.IgnoreCapacity):
		why := "the cluster autoscaler should add nodes for the pods that do not fit"
		if !in.Cluster.Autoscaling {
			why = "--ignore-capacity was given; pods that do not fit stay Pending"
		}
		r.Severity = Warn
		r.Message = "not enough room yet: " + binding + note + "; " + why
		r.Fix = "Check the autoscaler's node pool maximum allows the extra nodes, or add nodes to that pool."
	case worst > 1:
		r.Severity = Fail
		r.Message = "not enough room: " + binding + note
		r.Fix = "Add nodes to that pool, or disable products you do not use with --products. Pods that do not fit stay Pending. On a test cluster, --ignore-capacity installs anyway."
	case worst > capacityWarn:
		r.Severity = Warn
		r.Message = fmt.Sprintf("tight: %s, above the %.0f%% that leaves room for a rolling upgrade%s", binding, capacityWarn*100, note)
		r.Fix = "Add a node to that pool, or enable cluster autoscaling with room for at least one more node."
	default:
		r.Message = fmt.Sprintf("fits: the tightest is %s%s", binding, note)
	}
	return r
}

// totalCapacity compares total requests with total allocatable, for when the
// cluster's nodes or the release's pod groups are not known individually.
func totalCapacity(in Input) Result {
	r := Result{Code: "NT-PF-008", Check: "capacity", Severity: OK}
	have := fmt.Sprintf("%.1f vCPU / %.1f GiB allocatable", float64(in.Cluster.CPUMilli)/1000, float64(in.Cluster.Memory)/(1<<30))
	need := fmt.Sprintf("%.1f vCPU / %.1f GiB requested", float64(in.Needs.CPUMilli)/1000, float64(in.Needs.Memory)/(1<<30))
	r.Message = need + ", " + have
	if in.Cluster.CPUMilli < in.Needs.CPUMilli || in.Cluster.Memory < in.Needs.Memory {
		r.Severity = Warn
		r.Message = "not enough room: " + r.Message
		r.Fix = "Add worker nodes, or disable products you do not use with --products. Pods that do not fit stay Pending."
	}
	return r
}

// tolerates reports whether the tolerations admit every NoSchedule and
// NoExecute taint of a node, as the scheduler decides.
func tolerates(tols []corev1.Toleration, taints []corev1.Taint) bool {
	for _, t := range taints {
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		ok := false
		for _, tol := range tols {
			if tolerationMatches(tol, t) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func tolerationMatches(tol corev1.Toleration, t corev1.Taint) bool {
	if tol.Effect != "" && tol.Effect != t.Effect {
		return false
	}
	if tol.Key == "" {
		return tol.Operator == corev1.TolerationOpExists
	}
	if tol.Key != t.Key {
		return false
	}
	return tol.Operator == corev1.TolerationOpExists || tol.Value == t.Value
}

func selects(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func describeGroup(g manifest.PodGroup) string {
	what := "workloads with no node constraints"
	switch {
	case len(g.NodeSelector) > 0:
		var kv []string
		for k, v := range g.NodeSelector {
			kv = append(kv, k+"="+v)
		}
		sort.Strings(kv)
		what = "workloads selecting " + strings.Join(kv, ",")
	case len(g.Tolerations) > 0:
		var ks []string
		for _, t := range g.Tolerations {
			if t.Key != "" {
				ks = append(ks, t.Key)
			}
		}
		what = "workloads tolerating " + strings.Join(ks, ",")
	}
	if len(g.Workloads) <= 2 {
		return what + " (" + strings.Join(g.Workloads, ", ") + ")"
	}
	return fmt.Sprintf("%s (%s)", what, plural(len(g.Workloads), "workload"))
}

func ratio(need, room int64) float64 {
	if room <= 0 {
		if need > 0 {
			return 2
		}
		return 0
	}
	return float64(need) / float64(room)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func cores(milli int64) float64 { return float64(milli) / 1000 }
func gib(b int64) float64       { return float64(b) / (1 << 30) }

// Print writes results as a table, or as JSON when asJSON is set.
func Print(w io.Writer, rs []Result, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rs)
	}
	for _, r := range rs {
		fmt.Fprintf(w, "%-4s  %-9s %s\n", r.Severity, r.Check, r.Message)
		for _, d := range r.Detail {
			fmt.Fprintf(w, "      %-9s | %s\n", "", d)
		}
		if r.Fix != "" {
			fmt.Fprintf(w, "      %-9s fix (%s): %s\n", "", r.Code, r.Fix)
		}
	}
	return nil
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}
