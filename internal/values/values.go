// Package values builds the chart values ntctl installs with: a few generated
// keys from flags, with the operator's own values files layered on top.
package values

import (
	"fmt"
	"os"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"
	"sigs.k8s.io/yaml"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
)

// Products the chart knows, in the order they are reported.
var Products = []string{"trustgate", "trustguard", "dataPlane"}

// Secret names and keys the chart reads for the per-product tokens. They match
// values-required.yaml in the chart repo.
const (
	TrustGateConfigSync  = "agentgateway-config-sync"
	TrustGuardConfigSync = "trustguard-config-sync"
	TrustGateEnrolment   = "dataagent-enrolment-trustgate"
	TrustGuardEnrolment  = "dataagent-enrolment-trustguard"
	ConfigSyncKey        = "CONFIG_SYNC_TOKEN"
	EnrolmentKey         = "ENROLMENT_TOKEN"
)

// Modes are the deployment modes the chart knows, the default first.
var Modes = []string{"hybrid", "external", "saas"}

// Inputs are the convenience flags. Empty fields generate nothing, so a render
// driven only by -f files matches `helm template` with the same files.
type Inputs struct {
	// Mode is global.deploymentMode. Only a hybrid data plane reads the
	// console's tokens, so the token Secret references are generated for
	// hybrid (or an unset mode) only.
	Mode     string
	Domain   string
	Platform string
	Products []string
	// Gateway, when set, exposes the endpoints as HTTPRoutes on that Gateway
	// instead of Ingresses.
	Gateway *gatewayapi.ParentRef
	// OperationalTelemetry, when set, turns the gateway's and TrustGuard's own
	// traces and metrics on or off.
	OperationalTelemetry *bool
}

// Generate returns the values the flags stand for.
func Generate(in Inputs) map[string]any {
	global := map[string]any{}
	if in.Mode != "" {
		global["deploymentMode"] = in.Mode
	}
	if in.Domain != "" {
		global["domain"] = in.Domain
	}
	if in.Platform != "" {
		global["platform"] = in.Platform
	}
	if g := in.Gateway; g != nil {
		ref := map[string]any{"name": g.Name, "namespace": g.Namespace}
		if g.SectionName != "" {
			ref["sectionName"] = g.SectionName
		}
		global["ingress"] = map[string]any{
			"provider":   "gateway-api",
			"gatewayApi": map[string]any{"parentRefs": []any{ref}},
		}
	}
	if in.OperationalTelemetry != nil {
		global["observability"] = map[string]any{"operationalTelemetry": *in.OperationalTelemetry}
	}
	out := map[string]any{}
	if len(in.Products) > 0 {
		enabled := map[string]any{}
		for _, p := range Products {
			enabled[p] = contains(in.Products, p)
		}
		global["products"] = enabled
		hybrid := in.Mode == "" || in.Mode == "hybrid"
		if hybrid && contains(in.Products, "trustgate") {
			out["agentgateway"] = tokenRefs(TrustGateConfigSync, TrustGateEnrolment)
		}
		if hybrid && contains(in.Products, "trustguard") {
			out["trustguard"] = tokenRefs(TrustGuardConfigSync, TrustGuardEnrolment)
		}
	}
	if len(global) > 0 {
		out["global"] = global
	}
	return out
}

func tokenRefs(configSync, enrolment string) map[string]any {
	return map[string]any{
		"configSync": secretRef(configSync),
		"dataagent":  map[string]any{"enrolment": secretRef(enrolment)},
	}
}

func secretRef(name string) map[string]any {
	return map[string]any{"existingSecret": map[string]any{"name": name}}
}

// ProductDelta returns the values that turn the products in add on and those
// in remove off, leaving every other product as it is: what an upgrade applies
// over the release's own values. In hybrid an added TrustGate or TrustGuard
// reads ntctl's token Secrets, unless current already names Secrets of its
// own; other modes need no token.
func ProductDelta(current map[string]any, add, remove []string) map[string]any {
	out := map[string]any{}
	if len(add) == 0 && len(remove) == 0 {
		return out
	}
	products := map[string]any{}
	for _, p := range add {
		products[p] = true
	}
	for _, p := range remove {
		products[p] = false
	}
	out["global"] = map[string]any{"products": products}
	if DeploymentMode(current) != "hybrid" {
		return out
	}
	for _, p := range add {
		var root, configSync, enrolment string
		switch p {
		case "trustgate":
			root, configSync, enrolment = "agentgateway", TrustGateConfigSync, TrustGateEnrolment
		case "trustguard":
			root, configSync, enrolment = "trustguard", TrustGuardConfigSync, TrustGuardEnrolment
		default:
			continue
		}
		refs := map[string]any{}
		if !named(current, root, "configSync", "existingSecret", "name") {
			refs["configSync"] = secretRef(configSync)
		}
		if !named(current, root, "dataagent", "enrolment", "existingSecret", "name") {
			refs["dataagent"] = map[string]any{"enrolment": secretRef(enrolment)}
		}
		if len(refs) > 0 {
			out[root] = refs
		}
	}
	return out
}

func named(vals map[string]any, path ...string) bool {
	s, _ := Get(vals, path...).(string)
	return s != ""
}

// Files reads values files in order, later files winning, as helm -f does.
func Files(settings *cli.EnvSettings, files []string) (map[string]any, error) {
	opts := &values.Options{ValueFiles: files}
	vals, err := opts.MergeValues(getter.All(settings))
	if err != nil {
		return nil, fmt.Errorf("reading values files: %w", err)
	}
	return vals, nil
}

// Merge deep-merges override onto base and returns base.
func Merge(base, override map[string]any) map[string]any {
	for k, v := range override {
		if vm, ok := v.(map[string]any); ok {
			if bm, ok := base[k].(map[string]any); ok {
				base[k] = Merge(bm, vm)
				continue
			}
		}
		base[k] = v
	}
	return base
}

// Get returns the value at a dotted path, or nil.
func Get(vals map[string]any, path ...string) any {
	var cur any = vals
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// DeploymentMode returns global.deploymentMode, or the chart's default,
// hybrid.
func DeploymentMode(vals map[string]any) string {
	if m, _ := Get(vals, "global", "deploymentMode").(string); m != "" {
		return m
	}
	return "hybrid"
}

// Installed lists the products the chart deploys for these values, by the
// chart's own rule: in hybrid a product is on only when its flag is true; in
// external and saas it is on unless its flag is false.
func Installed(vals map[string]any) []string {
	hybrid := DeploymentMode(vals) == "hybrid"
	var out []string
	for _, p := range Products {
		on, set := Get(vals, "global", "products", p).(bool)
		if (hybrid && on) || (!hybrid && (on || !set)) {
			out = append(out, p)
		}
	}
	return out
}

// OperationalTelemetry reads global.observability.operationalTelemetry. set is
// false when the values do not record a choice, which the chart takes as off.
func OperationalTelemetry(vals map[string]any) (on, set bool) {
	v := Get(vals, "global", "observability", "operationalTelemetry")
	on, _ = v.(bool)
	return on, v != nil
}

// TelemetryState says whether operational telemetry is on and, when it is,
// where it goes: to NeuralTrust in hybrid installs, otherwise to the
// install's own in-cluster collector.
func TelemetryState(vals map[string]any) string {
	switch on, _ := OperationalTelemetry(vals); {
	case !on:
		return "off"
	case DeploymentMode(vals) == "hybrid":
		return "on, sent to NeuralTrust"
	default:
		return "on, sent to your in-cluster collector"
	}
}

// Save writes the values the operator would pass to helm. They hold no
// credentials: tokens live only in Kubernetes Secrets.
func Save(path string, vals map[string]any) error {
	b, err := yaml.Marshal(vals)
	if err != nil {
		return fmt.Errorf("encoding values: %w", err)
	}
	header := []byte("# Values for neuraltrust-platform, written by ntctl. No credentials are stored here.\n" +
		"# Use with: helm upgrade --install neuraltrust-platform <chart> -f " + path + "\n")
	if err := os.WriteFile(path, append(header, b...), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
