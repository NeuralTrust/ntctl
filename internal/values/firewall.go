package values

import "github.com/Masterminds/semver/v3"

// The Secret ntctl stores a TrustGuard's FIREWALL_JWT in, for the
// NeuralTrust-hosted firewall.
const (
	FirewallJWTSecret = "trustguard-firewall"
	FirewallJWTKey    = "FIREWALL_JWT"
)

// hostedSince is the first chart where an unset global.firewall.deploy means
// the NeuralTrust-hosted firewall instead of one in the cluster.
var hostedSince = semver.MustParse("4.0.0")

// HostedFirewallDefault reports whether chart version v sends TrustGuard to
// the NeuralTrust-hosted firewall unless the values choose otherwise. A
// version that does not parse is taken as the newest behaviour.
func HostedFirewallDefault(v string) bool {
	sv, err := semver.NewVersion(v)
	if err != nil {
		return true
	}
	return !sv.LessThan(hostedSince)
}

// FirewallSettled reports whether the values choose where TrustGuard's
// firewall runs: global.firewall.deploy set either way, a firewall the
// operator runs (global.firewall.baseURL), or the hosted firewall's
// FIREWALL_JWT (trustguard.firewall.jwt or jwtExistingSecret.name).
func FirewallSettled(vals map[string]any) bool {
	return Get(vals, "global", "firewall", "deploy") != nil ||
		named(vals, "global", "firewall", "baseURL") ||
		named(vals, "trustguard", "firewall", "jwt") ||
		named(vals, "trustguard", "firewall", "jwtExistingSecret", "name")
}

// NeedsFirewallChoice reports whether chart version v, rendered with vals,
// needs a firewall choice the values do not make: TrustGuard is on, the chart
// defaults to the hosted firewall, and nothing says where the firewall is.
// Rendered as is, such values fail on the missing FIREWALL_JWT.
func NeedsFirewallChoice(v string, vals map[string]any) bool {
	return HostedFirewallDefault(v) && contains(Installed(vals), "trustguard") && !FirewallSettled(vals)
}

// KeepFirewall returns the values that keep the release's firewall where it
// runs when the release moves to chart version to, or nil when there is
// nothing to keep. Before 4.0.0 an unset global.firewall.deploy ran the
// firewall in the cluster; from 4.0.0 it means the hosted firewall, which
// needs a FIREWALL_JWT the release never had. So an upgrade that leaves the
// choice open pins the firewall in the cluster, as it runs today.
func KeepFirewall(to string, vals map[string]any) map[string]any {
	if !NeedsFirewallChoice(to, vals) {
		return nil
	}
	return InClusterFirewall()
}

// PinFirewall returns the values that keep a release on chart version v
// running its firewall where it does once it moves to a 4.x chart, or nil
// when nothing needs pinning: the release is already on 4.x, runs no
// TrustGuard, or its values settle the firewall. On a 3.x chart the pin
// renders exactly as before, since an unset deploy already meant true.
func PinFirewall(v string, vals map[string]any) map[string]any {
	if HostedFirewallDefault(v) || !contains(Installed(vals), "trustguard") || FirewallSettled(vals) {
		return nil
	}
	return InClusterFirewall()
}

// InClusterFirewall runs the firewall in this cluster.
func InClusterFirewall() map[string]any {
	return map[string]any{"global": map[string]any{"firewall": map[string]any{"deploy": true}}}
}

// HostedFirewall points TrustGuard at the NeuralTrust-hosted firewall with the
// FIREWALL_JWT in ntctl's Secret.
func HostedFirewall() map[string]any {
	return map[string]any{"trustguard": map[string]any{"firewall": map[string]any{
		"jwtExistingSecret": map[string]any{"name": FirewallJWTSecret, "key": FirewallJWTKey},
	}}}
}
