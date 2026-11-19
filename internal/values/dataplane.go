package values

import "github.com/Masterminds/semver/v3"

// DefaultProducts are the products a new install turns on when the flags,
// files and answers leave the choice open. dataPlane (data-plane-api, the
// TrustTest red-teaming and evaluation API) is opt-in in every mode.
var DefaultProducts = []string{"trustgate", "trustguard"}

// dataPlaneOptInSince is the first chart where data-plane-api is opt-in in
// external and saas too: before it, an unset global.products.dataPlane
// deployed it there.
var dataPlaneOptInSince = semver.MustParse("4.5.1")

// DataPlaneOptIn reports whether chart version v deploys data-plane-api only
// when global.products.dataPlane is true, in every mode. A version that does
// not parse is taken as the newest behaviour.
func DataPlaneOptIn(v string) bool {
	sv, err := semver.NewVersion(v)
	if err != nil {
		return true
	}
	return !sv.LessThan(dataPlaneOptInSince)
}

// dataPlaneImplicit reports whether vals run data-plane-api on a chart older
// than dataPlaneOptInSince only because the flag is unset: external and saas
// deployed every product unless its flag was false.
func dataPlaneImplicit(vals map[string]any) bool {
	if DeploymentMode(vals) == "hybrid" {
		return false
	}
	_, set := Get(vals, "global", "products", "dataPlane").(bool)
	return !set
}

// KeepDataPlane returns the values that keep data-plane-api running when a
// release moves from chart version from to chart version to, or nil when
// there is nothing to keep. An external or saas release that never set
// global.products.dataPlane runs it on charts before dataPlaneOptInSince and
// would lose it on the move, so the upgrade pins it on, as it runs today.
func KeepDataPlane(from, to string, vals map[string]any) map[string]any {
	if DataPlaneOptIn(from) || !DataPlaneOptIn(to) || !dataPlaneImplicit(vals) {
		return nil
	}
	return DataPlaneOn()
}

// PinDataPlane returns the values that keep a release on chart version v
// running data-plane-api once it moves to a chart where it is opt-in, or nil
// when nothing needs pinning. On chart v the pin renders exactly as before.
func PinDataPlane(v string, vals map[string]any) map[string]any {
	if DataPlaneOptIn(v) || !dataPlaneImplicit(vals) {
		return nil
	}
	return DataPlaneOn()
}

// DataPlaneOn turns data-plane-api on.
func DataPlaneOn() map[string]any {
	return map[string]any{"global": map[string]any{"products": map[string]any{"dataPlane": true}}}
}
