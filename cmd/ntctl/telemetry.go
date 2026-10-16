package main

import (
	"fmt"
	"io"

	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// telemetryUse says what operational telemetry sends and where it goes in a
// deployment mode.
func telemetryUse(mode string) string {
	const what = "TrustGate and TrustGuard send their own traces and metrics\n(request rate, errors, latency) "
	if mode == "hybrid" {
		return what + "to NeuralTrust, to help support diagnose your gateway. Recommended."
	}
	return what + "to your own in-cluster collector."
}

// askTelemetry asks whether to turn operational telemetry on. The answer
// defaults to no: in hybrid installs it sends data to NeuralTrust, which the
// operator has to agree to.
func askTelemetry(in *prompt.Prompter, out io.Writer, mode string) (bool, error) {
	fmt.Fprintf(out, "Operational telemetry: %s\nChange it later with `ntctl upgrade --operational-telemetry=true --version <installed version>` (or =false).\n", telemetryUse(mode))
	return in.YesNo("Turn on operational telemetry?", false)
}

// upgradeTelemetry settles operational telemetry for an upgrade of a release
// with these values. The flag or an -f file wins; otherwise the release keeps
// what it records. A release that never recorded a choice is asked about once,
// on a terminal, and the answer is recorded; without a terminal it stays off.
func (o *opts) upgradeTelemetry(in *prompt.Prompter, out io.Writer, current map[string]any) error {
	if o.telemetry.v != nil || !in.Interactive {
		return nil
	}
	if _, set := values.OperationalTelemetry(current); set {
		return nil
	}
	files, err := values.Files(o.kube.Settings(), o.valueFiles)
	if err != nil {
		return err
	}
	if _, set := values.OperationalTelemetry(files); set {
		return nil
	}
	on, err := askTelemetry(in, out, values.DeploymentMode(reuse(current, files)))
	if err != nil {
		return err
	}
	o.telemetry.v = &on
	return nil
}
