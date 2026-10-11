package preflight

import (
	"context"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/manifest"
)

func lookup(existing map[string][]string) SecretLookup {
	return func(_ context.Context, name string) ([]string, bool, error) {
		keys, ok := existing[name]
		return keys, ok, nil
	}
}

func base() Input {
	return Input{
		Mode: ForInstall, Release: "neuraltrust-platform", Namespace: "neuraltrust",
		Cluster: &detect.Cluster{
			Version: "v1.33.0", Platform: "aws", Nodes: 3, CPUMilli: 24000, Memory: 96 << 30,
			IngressClasses: []string{"nginx"}, DefaultIngressClass: "nginx",
			StorageClasses: []string{"gp3"}, DefaultStorageClass: "gp3",
		},
		Needs: manifest.Needs{
			Secrets:        []manifest.SecretRef{{Name: "agentgateway-config-sync", Key: "CONFIG_SYNC_TOKEN", By: "Deployment/x"}},
			PullSecrets:    []string{"gcr-secret"},
			DefaultStorage: true, DefaultIngress: true,
			CPUMilli: 10000, Memory: 28 << 30,
		},
		Platform: "aws",
		Secrets:  lookup(map[string][]string{}),
	}
}

func severities(t *testing.T, in Input) map[string]Severity {
	t.Helper()
	rs, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Severity{}
	for _, r := range rs {
		out[r.Check] = r.Severity
		if r.Severity != OK && r.Fix == "" {
			t.Errorf("%s is %s without a fix", r.Check, r.Severity)
		}
	}
	return out
}

func TestHealthyInstall(t *testing.T) {
	in := base()
	in.WillCreate = map[string][]string{"agentgateway-config-sync": {"CONFIG_SYNC_TOKEN"}, "gcr-secret": {".dockerconfigjson"}}
	for check, sev := range severities(t, in) {
		if sev != OK {
			t.Errorf("%s = %s, want OK", check, sev)
		}
	}
}

func TestFailures(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*Input)
		check string
		want  Severity
	}{
		{"existing release blocks install", func(in *Input) { in.ReleaseExists = true }, "release", Fail},
		{"missing release blocks upgrade", func(in *Input) { in.Mode = ForUpgrade }, "release", Fail},
		{"no default storage class", func(in *Input) { in.Cluster.DefaultStorageClass = "" }, "storage", Fail},
		{"unknown storage class", func(in *Input) { in.Needs.StorageClasses = []string{"fast"} }, "storage", Fail},
		{"no ingress controller", func(in *Input) { in.Cluster.IngressClasses, in.Cluster.DefaultIngressClass = nil, "" }, "ingress", Warn},
		{"GKE serves classless Ingresses", func(in *Input) {
			in.Cluster.Platform, in.Platform = "gcp", "gcp"
			in.Cluster.IngressClasses, in.Cluster.DefaultIngressClass = nil, ""
		}, "ingress", OK},
		{"routes on a non-OpenShift cluster", func(in *Input) { in.Needs.Routes = true }, "ingress", Fail},
		{"missing secret", func(in *Input) {}, "secrets", Fail},
		{"secret missing its key", func(in *Input) {
			in.Secrets = lookup(map[string][]string{"agentgateway-config-sync": {"OTHER"}, "gcr-secret": {".dockerconfigjson"}})
		}, "secrets", Fail},
		{"too small", func(in *Input) { in.Cluster.CPUMilli = 4000 }, "capacity", Warn},
		{"platform mismatch", func(in *Input) { in.Platform = "gcp" }, "platform", Warn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			if tt.check != "secrets" {
				in.WillCreate = map[string][]string{"agentgateway-config-sync": {"CONFIG_SYNC_TOKEN"}, "gcr-secret": {".dockerconfigjson"}}
			}
			tt.edit(&in)
			if got := severities(t, in)[tt.check]; got != tt.want {
				t.Errorf("%s = %s, want %s", tt.check, got, tt.want)
			}
		})
	}
}
