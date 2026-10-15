// Package preflight checks a cluster against what a rendered release needs,
// before anything is applied.
package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

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
	return rs, nil
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
		r.Fix = "Use `ntctl upgrade` to change an existing install."
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
		msg = fmt.Sprintf("%d of %d Secrets the release reads are present; ntctl install creates the other %d", len(names)-planned, len(names), planned)
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
		r.Fix = "`ntctl install` creates the registry and token Secrets from --registry-key and your console tokens. Create any other Secret your values reference in namespace " + in.Namespace + "."
	}
	return []Result{r}, nil
}

func capacity(in Input) Result {
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
