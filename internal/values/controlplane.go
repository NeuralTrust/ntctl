package values

import (
	"net"
	"net/url"
	"strings"
)

// regionDomains are the DNS suffixes of NeuralTrust's own control planes, as
// the chart derives them from global.saasRegion.
var regionDomains = map[string]string{"eu": "neuraltrust.ai", "us": "us.neuraltrust.ai"}

// dialPrefixes are the host names the chart dials under a control-plane
// domain: <prefix><domain>.
var dialPrefixes = []string{"agentgateway-configsync.", "trustguard-configsync.", "databridge.", "telemetry."}

// RegionDomain is the control-plane domain of a NeuralTrust region, or ""
// for a region the chart does not know.
func RegionDomain(region string) string {
	return regionDomains[strings.ToLower(strings.TrimSpace(region))]
}

// DialedControlPlane is the control-plane domain a hybrid data plane with
// these values dials: global.controlPlane.domain when set (explicit), else
// the domain of global.saasRegion, which defaults to eu.
func DialedControlPlane(vals map[string]any) (domain string, explicit bool) {
	if d, _ := Get(vals, "global", "controlPlane", "domain").(string); strings.TrimSpace(d) != "" {
		return strings.TrimSpace(d), true
	}
	region, _ := Get(vals, "global", "saasRegion").(string)
	if region == "" {
		region = "eu"
	}
	return RegionDomain(region), false
}

// ControlPlaneFromAddr derives the control-plane domain from an address the
// console reports for it, such as agentgateway-configsync.<domain>:443 or
// databridge.<domain>:443. ok is false when the address does not follow the
// chart's naming, e.g. a raw load-balancer host.
func ControlPlaneFromAddr(addr string) (domain string, ok bool) {
	host := strings.TrimSpace(addr)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range dialPrefixes {
		if d, found := strings.CutPrefix(host, p); found && strings.Contains(d, ".") {
			return d, true
		}
	}
	return "", false
}

// ControlPlaneFromConsole guesses the control-plane domain of a console from
// its URL: NeuralTrust consoles live at app.<domain>. ok is false for a
// console under any other name.
func ControlPlaneFromConsole(appURL string) (domain string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(appURL))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	d, found := strings.CutPrefix(strings.ToLower(u.Hostname()), "app.")
	if !found || !strings.Contains(d, ".") {
		return "", false
	}
	return d, true
}

// ControlPlaneValues are the values that point a hybrid data plane at the
// control plane under domain.
func ControlPlaneValues(domain string) map[string]any {
	return map[string]any{"global": map[string]any{"controlPlane": map[string]any{"domain": domain}}}
}
