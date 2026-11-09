package values

import (
	"reflect"
	"testing"
)

func TestHostedFirewallDefault(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"3.15.1": false, "3.0.0": false, "4.0.0": true, "4.0.1": true, "5.2.0": true, "not-a-version": true,
	} {
		if got := HostedFirewallDefault(v); got != want {
			t.Errorf("HostedFirewallDefault(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestKeepFirewall(t *testing.T) {
	t.Parallel()
	hybridGuard := map[string]any{"global": map[string]any{"products": map[string]any{"trustguard": true}}}
	pinned := InClusterFirewall()
	cases := []struct {
		name string
		to   string
		vals map[string]any
		want map[string]any
	}{
		{"3.x to 4.x, TrustGuard, nothing chosen: pinned in the cluster", "4.0.1", hybridGuard, pinned},
		{"external mode runs TrustGuard unless it is off", "4.0.1", map[string]any{"global": map[string]any{"deploymentMode": "external"}}, pinned},
		{"staying on 3.x changes nothing", "3.15.1", hybridGuard, nil},
		{"no TrustGuard, no firewall", "4.0.1", map[string]any{"global": map[string]any{"products": map[string]any{"trustgate": true}}}, nil},
		{"deploy already chosen", "4.0.1", merge(hybridGuard, map[string]any{"global": map[string]any{"firewall": map[string]any{"deploy": false}}}), nil},
		{"a firewall the operator runs", "4.0.1", merge(hybridGuard, map[string]any{"global": map[string]any{"firewall": map[string]any{"deploy": false, "baseURL": "http://fw"}}}), nil},
		{"already on the hosted firewall", "4.0.1", merge(hybridGuard, HostedFirewall()), nil},
		{"inline JWT", "4.0.1", merge(hybridGuard, map[string]any{"trustguard": map[string]any{"firewall": map[string]any{"jwt": "x"}}}), nil},
	}
	for _, c := range cases {
		if got := KeepFirewall(c.to, c.vals); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: KeepFirewall = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPinFirewall(t *testing.T) {
	t.Parallel()
	hybridGuard := map[string]any{"global": map[string]any{"products": map[string]any{"trustguard": true}}}
	if got := PinFirewall("3.15.1", hybridGuard); !reflect.DeepEqual(got, InClusterFirewall()) {
		t.Errorf("a 3.x TrustGuard release is not pinned: %v", got)
	}
	if got := PinFirewall("4.0.1", hybridGuard); got != nil {
		t.Errorf("a 4.x release was pinned: %v", got)
	}
	if got := PinFirewall("3.15.1", map[string]any{"global": map[string]any{"products": map[string]any{"trustgate": true}}}); got != nil {
		t.Errorf("a release without TrustGuard was pinned: %v", got)
	}
}

// merge layers b over a copy of a, leaving both untouched.
func merge(a, b map[string]any) map[string]any {
	return Merge(Merge(map[string]any{}, clone(a)), clone(b))
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if mm, ok := v.(map[string]any); ok {
			v = clone(mm)
		}
		out[k] = v
	}
	return out
}
