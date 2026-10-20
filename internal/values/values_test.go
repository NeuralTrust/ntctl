package values

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
)

func TestGenerateMatchesValuesRequired(t *testing.T) {
	got := Generate(Inputs{Domain: "platform.example.com", Platform: "kubernetes", Products: Products})
	want := map[string]any{
		"global": map[string]any{
			"domain":   "platform.example.com",
			"platform": "kubernetes",
			"products": map[string]any{"trustgate": true, "trustguard": true, "dataPlane": true},
		},
		"agentgateway": tokenRefs("agentgateway-config-sync", "dataagent-enrolment-trustgate"),
		"trustguard":   tokenRefs("trustguard-config-sync", "dataagent-enrolment-trustguard"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Generate =\n%v\nwant\n%v", got, want)
	}
}

func TestGenerateEmptyAddsNothing(t *testing.T) {
	if got := Generate(Inputs{}); len(got) != 0 {
		t.Fatalf("Generate(empty) = %v, want nothing so -f alone matches helm", got)
	}
}

func TestGenerateSingleProduct(t *testing.T) {
	got := Generate(Inputs{Products: []string{"trustguard"}})
	if Get(got, "global", "products", "trustgate") != false || Get(got, "global", "products", "trustguard") != true {
		t.Fatalf("products = %v", Get(got, "global", "products"))
	}
	if Get(got, "agentgateway") != nil {
		t.Fatal("TrustGate token refs generated for a TrustGuard-only install")
	}
}

func TestMergeDeep(t *testing.T) {
	base := map[string]any{"global": map[string]any{"domain": "a", "platform": "gcp"}, "x": 1}
	over := map[string]any{"global": map[string]any{"domain": "b"}, "y": 2}
	got := Merge(base, over)
	if Get(got, "global", "domain") != "b" || Get(got, "global", "platform") != "gcp" || got["x"] != 1 || got["y"] != 2 {
		t.Fatalf("Merge = %v", got)
	}
}

func TestSaveHasNoTokensAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.yaml")
	if err := Save(path, Generate(Inputs{Domain: "d.example.com", Products: Products})); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "agentgateway-config-sync") || strings.Contains(string(b), "token:") {
		t.Fatalf("saved values should reference Secrets, never hold tokens:\n%s", b)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestGenerateGateway(t *testing.T) {
	out := Generate(Inputs{Gateway: &gatewayapi.ParentRef{Namespace: "gateway-system", Name: "shared", SectionName: "https"}})
	if Get(out, "global", "ingress", "provider") != "gateway-api" {
		t.Fatalf("provider not set: %v", out)
	}
	refs, _ := Get(out, "global", "ingress", "gatewayApi", "parentRefs").([]any)
	ref, _ := refs[0].(map[string]any)
	if len(refs) != 1 || ref["namespace"] != "gateway-system" || ref["name"] != "shared" || ref["sectionName"] != "https" {
		t.Errorf("parentRefs = %v", refs)
	}
	noSection := Generate(Inputs{Gateway: &gatewayapi.ParentRef{Namespace: "gw", Name: "edge"}})
	refs, _ = Get(noSection, "global", "ingress", "gatewayApi", "parentRefs").([]any)
	if _, has := refs[0].(map[string]any)["sectionName"]; has {
		t.Error("an empty sectionName must be left out, so the route attaches to every listener")
	}
}

func TestOperationalTelemetry(t *testing.T) {
	on, off := true, false
	if got := Get(Generate(Inputs{OperationalTelemetry: &on}), "global", "observability", "operationalTelemetry"); got != true {
		t.Errorf("on = %v", got)
	}
	if got := Get(Generate(Inputs{OperationalTelemetry: &off}), "global", "observability", "operationalTelemetry"); got != false {
		t.Errorf("off = %v", got)
	}
	if _, set := OperationalTelemetry(Generate(Inputs{Domain: "d"})); set {
		t.Error("an unset flag recorded a choice")
	}
	for _, c := range []struct {
		vals map[string]any
		want string
	}{
		{map[string]any{}, "off"},
		{Generate(Inputs{OperationalTelemetry: &on}), "on, sent to NeuralTrust"},
		{Merge(Generate(Inputs{OperationalTelemetry: &on}), map[string]any{"global": map[string]any{"deploymentMode": "external"}}), "on, sent to your in-cluster collector"},
		{Merge(Generate(Inputs{OperationalTelemetry: &off}), map[string]any{"global": map[string]any{"deploymentMode": "hybrid"}}), "off"},
	} {
		if got := TelemetryState(c.vals); got != c.want {
			t.Errorf("TelemetryState(%v) = %q, want %q", c.vals, got, c.want)
		}
	}
}

func TestProductDeltaChangesOnlyTheNamedProducts(t *testing.T) {
	got := ProductDelta(map[string]any{}, []string{"trustguard"}, []string{"dataPlane"})
	want := map[string]any{
		"global":     map[string]any{"products": map[string]any{"trustguard": true, "dataPlane": false}},
		"trustguard": tokenRefs(TrustGuardConfigSync, TrustGuardEnrolment),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProductDelta =\n%v\nwant\n%v", got, want)
	}
	if got := ProductDelta(nil, nil, nil); len(got) != 0 {
		t.Fatalf("no change = %v, want nothing", got)
	}
}

func TestProductDeltaKeepsTheReleasesOwnSecretNames(t *testing.T) {
	current := map[string]any{"trustguard": map[string]any{"configSync": map[string]any{"existingSecret": map[string]any{"name": "my-sync"}}}}
	got := ProductDelta(current, []string{"trustguard"}, nil)
	if Get(got, "trustguard", "configSync") != nil {
		t.Errorf("replaced the release's own config-sync Secret: %v", Get(got, "trustguard"))
	}
	if Get(got, "trustguard", "dataagent", "enrolment", "existingSecret", "name") != TrustGuardEnrolment {
		t.Errorf("enrolment Secret not pointed at ntctl's: %v", Get(got, "trustguard"))
	}
}

func TestInstalledFollowsTheChartRule(t *testing.T) {
	products := func(mode string, flags map[string]any) map[string]any {
		g := map[string]any{"products": flags}
		if mode != "" {
			g["deploymentMode"] = mode
		}
		return map[string]any{"global": g}
	}
	cases := []struct {
		name string
		vals map[string]any
		want []string
	}{
		{"hybrid needs true", products("", map[string]any{"trustgate": true, "trustguard": false}), []string{"trustgate"}},
		{"hybrid unset is off", products("hybrid", map[string]any{}), nil},
		{"external unset is on", products("external", map[string]any{"trustguard": false}), []string{"trustgate", "dataPlane"}},
		{"saas is external", products("saas", map[string]any{"dataPlane": false}), []string{"trustgate", "trustguard"}},
	}
	for _, c := range cases {
		if got := Installed(c.vals); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Installed = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNoTokenRefsOutsideHybrid: external and saas data planes never read the
// console's tokens, so nothing points them at token Secrets.
func TestNoTokenRefsOutsideHybrid(t *testing.T) {
	for _, mode := range []string{"external", "saas"} {
		v := Generate(Inputs{Mode: mode, Domain: "d.example.com", Products: Products})
		if Get(v, "global", "deploymentMode") != mode {
			t.Errorf("%s: deploymentMode = %v", mode, Get(v, "global", "deploymentMode"))
		}
		if Get(v, "agentgateway") != nil || Get(v, "trustguard") != nil {
			t.Errorf("%s: token references generated: %v", mode, v)
		}
		delta := ProductDelta(map[string]any{"global": map[string]any{"deploymentMode": mode}}, []string{"trustguard"}, nil)
		if Get(delta, "trustguard") != nil || Get(delta, "global", "products", "trustguard") != true {
			t.Errorf("%s: delta = %v, want only the product switch", mode, delta)
		}
	}
	for _, mode := range []string{"", "hybrid"} {
		v := Generate(Inputs{Mode: mode, Products: []string{"trustgate"}})
		if Get(v, "agentgateway", "configSync", "existingSecret", "name") != TrustGateConfigSync {
			t.Errorf("mode %q: hybrid must reference the token Secrets: %v", mode, v)
		}
	}
}
