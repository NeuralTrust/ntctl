package values

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
