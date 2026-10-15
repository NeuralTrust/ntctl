package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
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

func gatewayInput() Input {
	in := base()
	in.Cluster.Platform, in.Platform = "azure", "azure"
	in.Cluster.IngressClasses, in.Cluster.DefaultIngressClass = nil, ""
	in.Cluster.GatewayAPI, in.Cluster.GatewaysRead = true, true
	in.Cluster.Gateways = []gatewayapi.Gateway{{
		Namespace: "kgateway-system", Name: "internal",
		Listeners: []gatewayapi.Listener{{Name: "http", From: "All"}, {Name: "private", From: "Same"}},
		Addresses: []gatewayapi.Address{{Type: "IPAddress", Value: "10.0.6.10"}},
	}}
	in.IngressProvider = "gateway-api"
	in.Needs.DefaultIngress = false
	in.Needs.HTTPRoutes = true
	in.Needs.Gateways = []gatewayapi.ParentRef{{Namespace: "kgateway-system", Name: "internal"}}
	return in
}

func ingressResult(t *testing.T, in Input) Result {
	t.Helper()
	rs, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Check == "ingress" {
			return r
		}
	}
	t.Fatal("no ingress result")
	return Result{}
}

func TestGatewayRoutes(t *testing.T) {
	r := ingressResult(t, gatewayInput())
	if r.Severity != OK || !strings.Contains(r.Message, "kgateway-system/internal (10.0.6.10)") {
		t.Errorf("healthy Gateway attach = %+v", r)
	}

	for name, tc := range map[string]struct {
		mutate func(*Input)
		want   Severity
		says   string
	}{
		"no Gateway API":      {func(in *Input) { in.Cluster.GatewayAPI = false }, Fail, "does not serve"},
		"Gateways unreadable": {func(in *Input) { in.Cluster.GatewaysRead = false }, Warn, "could not list"},
		"missing Gateway": {func(in *Input) {
			in.Needs.Gateways = []gatewayapi.ParentRef{{Namespace: "kgateway-system", Name: "external"}}
		}, Fail, "does not exist"},
		"missing listener":               {func(in *Input) { in.Needs.Gateways[0].SectionName = "https" }, Warn, `no listener "https"`},
		"listener refuses the namespace": {func(in *Input) { in.Needs.Gateways[0].SectionName = "private" }, Warn, "does not admit"},
		"parent in the release namespace": {func(in *Input) {
			in.Cluster.Gateways[0].Namespace = in.Namespace
			in.Cluster.Gateways[0].Listeners = []gatewayapi.Listener{{Name: "private", From: "Same"}}
			in.Needs.Gateways = []gatewayapi.ParentRef{{Name: "internal"}}
		}, OK, ""},
		"chart predates gateway-api": {func(in *Input) {
			in.Needs.HTTPRoutes, in.Needs.Gateways, in.Needs.DefaultIngress = false, nil, true
		}, Fail, "predates"},
	} {
		in := gatewayInput()
		tc.mutate(&in)
		r := ingressResult(t, in)
		got := r.Message + " " + strings.Join(r.Detail, " ")
		if r.Severity != tc.want || !strings.Contains(got, tc.says) {
			t.Errorf("%s: %s %q; want %s mentioning %q", name, r.Severity, got, tc.want, tc.says)
		}
		if r.Severity != OK && r.Fix == "" {
			t.Errorf("%s: %s without a fix", name, r.Severity)
		}
	}
}

func TestNoIngressControllerSuggestsGateway(t *testing.T) {
	in := gatewayInput()
	in.IngressProvider = ""
	in.Needs.HTTPRoutes, in.Needs.Gateways, in.Needs.DefaultIngress = false, nil, true
	r := ingressResult(t, in)
	if r.Severity != Warn || !strings.Contains(r.Fix, "--gateway kgateway-system/internal") {
		t.Errorf("result = %+v", r)
	}
}
