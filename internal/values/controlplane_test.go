package values

import "testing"

func TestControlPlaneFromAddr(t *testing.T) {
	cases := map[string]string{
		"agentgateway-configsync.cp.example.com:443":  "cp.example.com",
		"trustguard-configsync.us.neuraltrust.ai:443": "us.neuraltrust.ai",
		"databridge.neuraltrust.ai:443":               "neuraltrust.ai",
		"https://telemetry.cp.example.com":            "cp.example.com",
		"AGENTGATEWAY-CONFIGSYNC.CP.EXAMPLE.COM.":     "cp.example.com",
		"k8s-abc.elb.amazonaws.com:443":               "",
		"agentgateway-configsync.example:443":         "",
		"":                                            "",
	}
	for in, want := range cases {
		got, ok := ControlPlaneFromAddr(in)
		if got != want || ok != (want != "") {
			t.Errorf("ControlPlaneFromAddr(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestControlPlaneFromConsole(t *testing.T) {
	cases := map[string]string{
		"https://app.neuraltrust.ai":     "neuraltrust.ai",
		"https://app.us.neuraltrust.ai/": "us.neuraltrust.ai",
		"https://app.cp.example.com/":    "cp.example.com",
		"http://127.0.0.1:51175":         "",
		"https://console.example.com":    "",
		"https://app.localhost":          "",
		"not a url":                      "",
	}
	for in, want := range cases {
		got, ok := ControlPlaneFromConsole(in)
		if got != want || ok != (want != "") {
			t.Errorf("ControlPlaneFromConsole(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestDialedControlPlane(t *testing.T) {
	cases := []struct {
		vals     map[string]any
		want     string
		explicit bool
	}{
		{map[string]any{}, "neuraltrust.ai", false},
		{map[string]any{"global": map[string]any{"saasRegion": "us"}}, "us.neuraltrust.ai", false},
		{Merge(map[string]any{"global": map[string]any{"saasRegion": "us"}}, ControlPlaneValues("cp.example.com")), "cp.example.com", true},
	}
	for _, c := range cases {
		got, explicit := DialedControlPlane(c.vals)
		if got != c.want || explicit != c.explicit {
			t.Errorf("DialedControlPlane(%v) = %q, %v; want %q, %v", c.vals, got, explicit, c.want, c.explicit)
		}
	}
}
