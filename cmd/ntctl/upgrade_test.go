package main

import (
	"reflect"
	"testing"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/redact"
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
