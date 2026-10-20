package gatewayapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi/gatewayapitest"
)

func TestParseRef(t *testing.T) {
	for in, want := range map[string]gatewayapi.ParentRef{
		"gateway-system/shared":       {Namespace: "gateway-system", Name: "shared"},
		"gateway-system/shared:https": {Namespace: "gateway-system", Name: "shared", SectionName: "https"},
		" gw/edge ":                   {Namespace: "gw", Name: "edge"},
	} {
		got, err := gatewayapi.ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "shared", "/shared", "gw/", "gw/shared:", "a/b/c"} {
		if _, err := gatewayapi.ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

func TestListGateways(t *testing.T) {
	dyn := gatewayapitest.Client(
		gatewayapitest.Gateway("gw", "shared", "192.0.2.10",
			gatewayapitest.Listener("http", 80, map[string]any{"from": "All"}),
			gatewayapitest.Listener("https", 443, nil),
			gatewayapitest.Listener("team", 8443, map[string]any{"from": "Selector", "selector": map[string]any{
				"matchLabels": map[string]any{"team": "ai"},
			}}),
		),
	)
	gws, err := gatewayapi.ListGateways(context.Background(), dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(gws) != 1 || gws[0].Ref() != "gw/shared" {
		t.Fatalf("gateways = %+v", gws)
	}
	g := gws[0]
	if typ, target := g.DNS(); typ != "A" || target != "192.0.2.10" {
		t.Errorf("DNS() = %s %s", typ, target)
	}
	all, _ := g.Listener("http")
	same, _ := g.Listener("https")
	team, _ := g.Listener("team")
	if !all.Admits("gw", "nt", nil) {
		t.Error("from: All refused another namespace")
	}
	if same.From != "Same" || same.Admits("gw", "nt", nil) || !same.Admits("gw", "gw", nil) {
		t.Errorf("unset from must mean Same: %+v", same)
	}
	if !team.Admits("gw", "nt", map[string]string{"team": "ai"}) || team.Admits("gw", "nt", map[string]string{"team": "web"}) {
		t.Error("from: Selector did not follow the namespace labels")
	}
	if _, ok := g.Listener("missing"); ok {
		t.Error("found a listener that does not exist")
	}
}

func TestGetGatewayMissing(t *testing.T) {
	g, err := gatewayapi.GetGateway(context.Background(), gatewayapitest.Client(), "gw", "nope")
	if err != nil || g != nil {
		t.Errorf("GetGateway on a missing Gateway = %+v, %v; want nil, nil", g, err)
	}
}

func TestListRoutes(t *testing.T) {
	parent := map[string]any{"name": "shared", "namespace": "gw", "sectionName": "https"}
	local := map[string]any{"name": "local"}
	dyn := gatewayapitest.Client(
		gatewayapitest.Route("nt", "ok", []string{"app.example.com"}, []map[string]any{parent},
			[]map[string]any{gatewayapitest.ParentStatus(parent, "Accepted", "True", "Accepted")}),
		gatewayapitest.Route("nt", "refused", []string{"api.example.com"}, []map[string]any{parent},
			[]map[string]any{gatewayapitest.ParentStatus(parent, "Accepted", "False", "NotAllowedByListeners")}),
		gatewayapitest.Route("nt", "orphan", nil, []map[string]any{local}, nil),
		gatewayapitest.Route("other", "elsewhere", nil, nil, nil),
	)
	routes, err := gatewayapi.ListRoutes(context.Background(), dyn, "nt")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]gatewayapi.Route{}
	for _, r := range routes {
		byName[r.Name] = r
	}
	if len(routes) != 3 {
		t.Fatalf("routes = %+v; want the three in nt", routes)
	}
	if p := byName["orphan"].Parents[0]; p.Namespace != "nt" || p.Name != "local" {
		t.Errorf("a parent without a namespace must resolve to the route's: %+v", p)
	}
	if got := byName["ok"].Problems(); len(got) != 0 {
		t.Errorf("accepted route has problems: %v", got)
	}
	got := byName["refused"].Problems()
	if len(got) != 1 || !strings.Contains(got[0], "gw/shared:https: Accepted=False NotAllowedByListeners") {
		t.Errorf("refused route problems = %v", got)
	}
	if got := byName["orphan"].Problems(); len(got) != 1 || !strings.Contains(got[0], "no Gateway controller") {
		t.Errorf("unreported route problems = %v", got)
	}
}
