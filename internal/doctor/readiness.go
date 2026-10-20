package doctor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/preflight"
)

// readinessFix is the fix of an NT-DR-015 whose cause is not recognised.
const readinessFix = "The readiness probe's failures and the container's last errors are above. `ntctl support-bundle` collects the full logs for NeuralTrust support."

// Hopeless reports whether a finding is one that waiting does not fix: an
// image that cannot be pulled, a Secret or ConfigMap that is missing, a
// container that keeps crashing, or one that never becomes ready for a cause
// doctor recognises (such as a token another control plane issued). Pods
// that cannot be scheduled are not hopeless: an autoscaler may add nodes.
func Hopeless(r preflight.Result) bool {
	if r.Severity != preflight.Fail {
		return false
	}
	switch r.Code {
	case "NT-DR-010", "NT-DR-011", "NT-DR-012":
		return true
	case "NT-DR-015":
		return r.Fix != readinessFix
	}
	return false
}

// readyGrace is how long a running container may stay unready before it is
// a finding: long enough for a normal start, probes included.
const readyGrace = 90 * time.Second

// notReady reports whether container c of pod p has been running for longer
// than readyGrace without becoming ready. Init containers are left out: they
// are never ready, they finish.
func notReady(p corev1.Pod, c corev1.ContainerStatus, now time.Time) bool {
	if p.Status.Phase != corev1.PodRunning || c.Ready || c.State.Running == nil || p.DeletionTimestamp != nil {
		return false
	}
	for _, ic := range p.Status.InitContainerStatuses {
		if ic.Name == c.Name {
			return false
		}
	}
	return now.Sub(c.State.Running.StartedAt.Time) > readyGrace
}

// filter keeps the lines that start with one of the prefixes.
func filter(lines []string, prefixes ...string) []string {
	var out []string
	for _, l := range lines {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// notableLines picks what explains a failure from a container's log: from
// JSON lines, those at warn level or above, as "msg: error"; other lines as
// they are. A line repeated (a retry loop) is kept once, at its last place.
// It returns the last n.
func notableLines(lines []string, n int) []string {
	var picked []string
	for _, l := range lines {
		text, keep := logText(l)
		if keep {
			picked = append(picked, text)
		}
	}
	seen := map[string]bool{}
	var out []string
	for i := len(picked) - 1; i >= 0 && len(out) < n; i-- {
		if !seen[picked[i]] {
			seen[picked[i]] = true
			out = append(out, picked[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// logText reads one log line: a structured one is reduced to its level,
// message and error, and kept only at warn level or above.
func logText(line string) (string, bool) {
	var rec map[string]any
	if !strings.HasPrefix(strings.TrimSpace(line), "{") || json.Unmarshal([]byte(line), &rec) != nil {
		return clip(line, 300), strings.TrimSpace(line) != ""
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := rec[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch strings.ToLower(str("level", "severity", "lvl")) {
	case "warn", "warning", "error", "fatal", "panic", "critical":
	default:
		return "", false
	}
	text := strings.ToUpper(str("level", "severity", "lvl")) + " " + str("msg", "message")
	if e := str("error", "err"); e != "" {
		text += ": " + e
	}
	return clip(text, 300), true
}

// knownCause returns the fix for a failure whose log lines name a cause
// seen before, or "" when none matches. lines are read whole; the fix never
// quotes them. env is the container's environment, for the addresses a fix
// can name.
func knownCause(lines []string, env map[string]string) string {
	text := strings.Join(lines, "\n")
	has := func(subs ...string) bool {
		for _, s := range subs {
			if !strings.Contains(text, s) {
				return false
			}
		}
		return true
	}
	switch {
	case has("configsync", "Unauthenticated"):
		at := "the control plane it dials"
		if ep := env["CONFIG_SYNC_GRPC_ENDPOINT"]; ep != "" {
			at = ep
		}
		return fmt.Sprintf("Config sync at %s refuses this data plane's token: the token was issued by the console of another control plane. "+
			"Point the data plane at the control plane whose console issued the token: `ntctl upgrade --control-plane <its domain> --version <installed version>` "+
			"(for a console at app.<domain>, that is <domain>). Or put tokens from the console of the control plane at %s in the token Secrets and run `ntctl upgrade`.", at, at)
	case has("password authentication failed"):
		return "PostgreSQL refuses the user or password: check the password in the external-postgresql Secret (POSTGRES_PASSWORD) and the user in your values, then run `ntctl upgrade`."
	case has("WRONGPASS"), has("NOAUTH"), has("invalid username-password pair"):
		return "Redis refuses the credentials: check REDIS_PASSWORD (and REDIS_USERNAME) in the external-redis Secret, then run `ntctl upgrade`."
	case has("x509:"):
		return "A TLS certificate is not trusted (see the x509 error above): with a private CA, give its bundle to the chart (global.controlPlane.caSecretName for the control plane), or use the server's public certificate."
	case has("no such host"):
		return "A host name does not resolve (see above): check the DNS records and the addresses in your values."
	case has("connection refused"), has("i/o timeout"):
		return "A dependency cannot be reached (see above): check that it runs, and that network policies, firewalls and egress allow the connection."
	}
	return ""
}

// env returns the literal environment variables of a container in a pod's
// spec; values read from Secrets and ConfigMaps are left out.
func env(p corev1.Pod, container string) map[string]string {
	out := map[string]string{}
	for _, c := range p.Spec.Containers {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				out[e.Name] = e.Value
			}
		}
	}
	return out
}
