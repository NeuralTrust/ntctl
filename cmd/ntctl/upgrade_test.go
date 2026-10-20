package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/redact"
	"github.com/NeuralTrust/ntctl/internal/values"
)

func rev(v int, st rcommon.Status) *release.Release {
	return &release.Release{Version: v, Info: &release.Info{Status: st}}
}

func TestRollbackTarget(t *testing.T) {
	hist := []*release.Release{
		rev(1, rcommon.StatusSuperseded), rev(2, rcommon.StatusFailed), rev(3, rcommon.StatusSuperseded), rev(4, rcommon.StatusFailed),
	}
	if got := rollbackTarget(hist, 0); got == nil || got.Version != 3 {
		t.Fatalf("default target = %v, want the newest earlier working revision 3", got)
	}
	if got := rollbackTarget(hist, 1); got == nil || got.Version != 1 {
		t.Fatalf("--to 1 = %v", got)
	}
	if got := rollbackTarget(hist, 9); got != nil {
		t.Fatalf("--to 9 = %v, want none", got)
	}
	if got := rollbackTarget(hist[:1], 0); got != nil {
		t.Fatalf("a single revision has nothing to return to, got %v", got)
	}
}

func TestAdoptRedactionLeavesTheReleaseValues(t *testing.T) {
	orig := map[string]any{
		"global":   map[string]any{"postgresql": map[string]any{"host": "db", "password": "s3cret"}},
		"extraEnv": []any{map[string]any{"name": "API_TOKEN", "value": "tok"}},
	}
	clean, _ := redact.Value(deepCopy(orig)).(map[string]any)
	got := redactedPaths(orig, clean, "")
	want := []string{"extraEnv[0].value", "global.postgresql.password"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redacted paths = %v, want %v", got, want)
	}
	if orig["global"].(map[string]any)["postgresql"].(map[string]any)["password"] != "s3cret" ||
		orig["extraEnv"].([]any)[0].(map[string]any)["value"] != "tok" {
		t.Fatal("redacting the copy changed the release's own values")
	}
}

// upgradeWith runs the upgrade's telemetry decision and returns the values the
// upgrade applies over the release's, with what it printed.
func upgradeWith(t *testing.T, o opts, in func(io.Writer) *prompt.Prompter, current map[string]any) (map[string]any, string) {
	t.Helper()
	var out bytes.Buffer
	if err := o.upgradeTelemetry(in(&out), &out, current); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	vals, err := o.effectiveValues(values.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	return reuse(current, vals), out.String()
}

func releaseValues(mode string, telemetry any) map[string]any {
	global := map[string]any{"domain": "d.example.com"}
	if mode != "" {
		global["deploymentMode"] = mode
	}
	if telemetry != nil {
		global["observability"] = map[string]any{"operationalTelemetry": telemetry}
	}
	return map[string]any{"global": global}
}

func TestUpgradeOperationalTelemetry(t *testing.T) {
	on, off := true, false
	// terminal answers as typed; with no input, any question fails on EOF.
	terminal := func(input string) func(io.Writer) *prompt.Prompter {
		return func(w io.Writer) *prompt.Prompter { return prompt.Scripted(strings.NewReader(input), w) }
	}
	noTerminal := func(w io.Writer) *prompt.Prompter {
		return prompt.New(context.Background(), strings.NewReader("y\n"), w, false)
	}

	cases := []struct {
		name    string
		o       opts
		in      func(io.Writer) *prompt.Prompter
		current map[string]any
		want    bool
		set     bool // the upgraded release records a choice
		asked   string
	}{
		{"flag on wins over the release", opts{telemetry: optionalBool{v: &on}}, terminal(""), releaseValues("", false), true, true, ""},
		{"flag off wins over the release", opts{telemetry: optionalBool{v: &off}}, terminal(""), releaseValues("", true), false, true, ""},
		{"unset keeps on", opts{}, terminal(""), releaseValues("", true), true, true, ""},
		{"unset keeps off", opts{}, terminal(""), releaseValues("", false), false, true, ""},
		{"never recorded, terminal, yes", opts{}, terminal("y\n"), releaseValues("", nil), true, true, "to NeuralTrust, to help support diagnose your gateway"},
		{"never recorded, terminal, Enter", opts{}, terminal("\n"), releaseValues("hybrid", nil), false, true, "[y/N]"},
		{"never recorded, external", opts{}, terminal("y\n"), releaseValues("external", nil), true, true, "to your own in-cluster collector"},
		{"never recorded, no terminal", opts{}, noTerminal, releaseValues("", nil), false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vals, out := upgradeWith(t, c.o, c.in, c.current)
			got, set := values.OperationalTelemetry(vals)
			if got != c.want || set != c.set {
				t.Errorf("operational telemetry = %v (recorded %v), want %v (recorded %v)", got, set, c.want, c.set)
			}
			if c.asked == "" && out != "" {
				t.Errorf("asked when the choice was settled:\n%s", out)
			}
			if c.asked != "" && !strings.Contains(out, c.asked) {
				t.Errorf("question does not say %q:\n%s", c.asked, out)
			}
			if got := values.Get(vals, "global", "domain"); got != "d.example.com" {
				t.Errorf("the release's other values were not kept: domain = %v", got)
			}
		})
	}
}

func TestUpgradeTelemetryFromFileIsNotAsked(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v.yaml")
	if err := os.WriteFile(file, []byte("global:\n  observability:\n    operationalTelemetry: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, out := upgradeWith(t, opts{valueFiles: stringList{file}},
		func(w io.Writer) *prompt.Prompter { return prompt.Scripted(strings.NewReader(""), w) }, releaseValues("", nil))
	if on, _ := values.OperationalTelemetry(vals); !on || out != "" {
		t.Errorf("an -f value should settle it without asking: on=%v\n%s", on, out)
	}
}
