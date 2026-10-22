package preflight

import (
	"context"
	"errors"
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
		Namespace: "gateway-system", Name: "shared",
		Listeners: []gatewayapi.Listener{{Name: "http", From: "All"}, {Name: "private", From: "Same"}},
		Addresses: []gatewayapi.Address{{Type: "IPAddress", Value: "192.0.2.10"}},
	}}
	in.IngressProvider = "gateway-api"
	in.Needs.DefaultIngress = false
	in.Needs.HTTPRoutes = true
	in.Needs.Gateways = []gatewayapi.ParentRef{{Namespace: "gateway-system", Name: "shared"}}
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
	if r.Severity != OK || !strings.Contains(r.Message, "gateway-system/shared (192.0.2.10)") {
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
			in.Needs.Gateways = []gatewayapi.ParentRef{{Namespace: "gateway-system", Name: "external"}}
		}, Fail, "does not exist"},
		"missing listener":               {func(in *Input) { in.Needs.Gateways[0].SectionName = "https" }, Warn, `no listener "https"`},
		"listener refuses the namespace": {func(in *Input) { in.Needs.Gateways[0].SectionName = "private" }, Warn, "does not admit"},
		"parent in the release namespace": {func(in *Input) {
			in.Cluster.Gateways[0].Namespace = in.Namespace
			in.Cluster.Gateways[0].Listeners = []gatewayapi.Listener{{Name: "private", From: "Same"}}
			in.Needs.Gateways = []gatewayapi.ParentRef{{Name: "shared"}}
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
	if r.Severity != Warn || !strings.Contains(r.Fix, "--gateway gateway-system/shared") {
		t.Errorf("result = %+v", r)
	}
}

func TestExposure(t *testing.T) {
	lb := func(name string, ann map[string]string) manifest.LoadBalancer {
		return manifest.LoadBalancer{Name: name, Annotations: ann}
	}
	cases := []struct {
		name, platform string
		lbs            []manifest.LoadBalancer
		want           Severity // "" when the check does not apply
		says           string
	}{
		{name: "no load balancers", platform: "azure"},
		{name: "unknown platform", platform: "kubernetes", lbs: []manifest.LoadBalancer{lb("databridge-southbound", nil)}},
		{name: "azure public", platform: "azure", lbs: []manifest.LoadBalancer{lb("databridge-southbound", nil), lb("agentgateway-admin-configsync", nil)}, want: Warn,
			says: "2 LoadBalancer Services get a public address on azure: agentgateway-admin-configsync, databridge-southbound"},
		{name: "azure internal", platform: "azure", lbs: []manifest.LoadBalancer{lb("databridge-southbound", map[string]string{"service.beta.kubernetes.io/azure-load-balancer-internal": "true"})}, want: OK},
		{name: "aws internal scheme", platform: "aws", lbs: []manifest.LoadBalancer{lb("databridge-southbound", map[string]string{"service.beta.kubernetes.io/aws-load-balancer-scheme": "internal"})}, want: OK},
		{name: "aws internet-facing", platform: "aws", lbs: []manifest.LoadBalancer{lb("databridge-southbound", map[string]string{"service.beta.kubernetes.io/aws-load-balancer-scheme": "internet-facing"})}, want: Warn},
		{name: "gke internal", platform: "gcp", lbs: []manifest.LoadBalancer{lb("x", map[string]string{"networking.gke.io/load-balancer-type": "Internal"})}, want: OK},
	}
	for _, c := range cases {
		r, ok := exposure(Input{Platform: c.platform, Needs: manifest.Needs{LoadBalancers: c.lbs}})
		switch {
		case c.want == "" && ok:
			t.Errorf("%s: should not apply, got %+v", c.name, r)
		case c.want != "" && (!ok || r.Severity != c.want):
			t.Errorf("%s: %+v, want %s", c.name, r, c.want)
		case c.says != "" && !strings.Contains(r.Message, c.says):
			t.Errorf("%s: message %q lacks %q", c.name, r.Message, c.says)
		case r.Severity == Warn && r.Fix == "":
			t.Errorf("%s: a warning without a fix", c.name)
		}
	}
}

func TestControlPlane(t *testing.T) {
	answers := map[string][]string{
		"agentgateway-configsync.cp.example.com":  {"203.0.113.10"},
		"databridge.cp.example.com":               {"203.0.113.11"},
		"agentgateway-configsync.vpn.example.com": {"100.110.196.14"},
		"databridge.vpn.example.com":              {"100.110.196.16", "203.0.113.12"},
	}
	resolve := func(_ context.Context, host string) ([]string, error) {
		if a, ok := answers[host]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	cases := []struct {
		name, mode string
		dials      []string
		want       Severity // "" when the check does not apply
		says       string
	}{
		{name: "not hybrid", mode: "external", dials: []string{"agentgateway-configsync.vpn.example.com:443"}},
		{name: "nothing dialled", mode: "hybrid"},
		{name: "public names", mode: "hybrid", dials: []string{"agentgateway-configsync.cp.example.com:443", "databridge.cp.example.com:443"}, want: OK,
			says: "the control plane resolves: agentgateway-configsync.cp.example.com, databridge.cp.example.com"},
		{name: "VPN only", mode: "hybrid", dials: []string{"agentgateway-configsync.vpn.example.com:443", "databridge.vpn.example.com:443"}, want: Warn,
			says: "1 of 2 names resolve only here or not at all"},
		{name: "unresolved", mode: "hybrid", dials: []string{"trustguard-configsync.gone.example.com:443"}, want: Warn,
			says: "1 of 1 names"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base()
			in.DeploymentMode, in.Resolve = c.mode, resolve
			in.Needs.Dials = c.dials
			r, ok := controlPlane(context.Background(), in)
			if c.want == "" {
				if ok {
					t.Fatalf("the check should not apply: %+v", r)
				}
				return
			}
			if !ok || r.Severity != c.want || !strings.Contains(r.Message, c.says) {
				t.Fatalf("result = %+v, want %s saying %q", r, c.want, c.says)
			}
			if c.want == Warn && (r.Fix == "" || len(r.Detail) == 0) {
				t.Errorf("a warning names the host and the fix: %+v", r)
			}
		})
	}
	in := base()
	in.DeploymentMode, in.Resolve = "hybrid", resolve
	in.Needs.Dials = []string{"agentgateway-configsync.vpn.example.com:443"}
	r, _ := controlPlane(context.Background(), in)
	if r.Detail[0] != "agentgateway-configsync.vpn.example.com resolves here only to 100.110.196.14, an address a VPN client such as Twingate or Tailscale hands out" {
		t.Errorf("detail = %q", r.Detail)
	}
}
