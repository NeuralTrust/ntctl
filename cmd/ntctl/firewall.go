package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/firewall"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// Where an install runs TrustGuard's firewall (--firewall).
const (
	firewallInCluster = "in-cluster"
	firewallHosted    = "hosted"
)

// firewallChoice settles where a new install runs TrustGuard's firewall when
// chart version v defaults to the NeuralTrust-hosted one and the values leave
// it open. Hosted needs the install's FIREWALL_JWT, from NT_FIREWALL_JWT, a
// trustguard-firewall Secret already in the namespace, or the terminal; ntctl
// stores a new one in that Secret. Without a terminal and with neither, the
// firewall runs in the cluster, as every 3.x install did, and warn says so.
// It returns the values to add and the Secret to create, either of which may
// be nil. Every FIREWALL_JWT is validated before it is used.
func firewallChoice(ctx context.Context, in *prompt.Prompter, warn io.Writer, cs kubernetes.Interface, ns string, f installFlags, v string, vals map[string]any) (map[string]any, *corev1.Secret, error) {
	if !values.NeedsFirewallChoice(v, vals) {
		return nil, nil, nil
	}
	fw, err := settleFirewall(ctx, cs, ns, f.firewall, os.Getenv("NT_FIREWALL_JWT"), f.replace)
	if err != nil || fw.settled {
		return fw.vals, fw.secret, err
	}
	choice := f.firewall
	if choice == "" {
		i, err := in.Choose("Where should TrustGuard's firewall run?", []string{
			"In this cluster",
			"The NeuralTrust-hosted firewall, with this install's FIREWALL_JWT",
		}, 0)
		if err != nil {
			return nil, nil, err
		}
		if i == 0 {
			if !in.Interactive {
				fmt.Fprintf(warn, "Warning: TrustGuard's firewall will run in this cluster, the default without a terminal. For the NeuralTrust-hosted firewall, set NT_FIREWALL_JWT (or create the Secret %s) and pass --firewall %s; pass --firewall %s to choose this explicitly.\n", values.FirewallJWTSecret, firewallHosted, firewallInCluster)
			}
			return values.InClusterFirewall(), nil, nil
		}
	}
	jwt, err := in.Secret("FIREWALL_JWT (shown by the console when it created the TrustGuard, or sent by NeuralTrust)")
	if err != nil {
		return nil, nil, fmt.Errorf("the hosted firewall needs this install's FIREWALL_JWT: set NT_FIREWALL_JWT, or pass --firewall %s: %w", firewallInCluster, err)
	}
	if err := firewall.Validate(jwt); err != nil {
		return nil, nil, err
	}
	return values.HostedFirewall(), firewallSecret(ns, jwt), nil
}

// settledFirewall is where TrustGuard's firewall runs when that can be decided
// without asking.
type settledFirewall struct {
	settled bool
	vals    map[string]any
	// secret is the trustguard-firewall Secret to create, or nil.
	secret *corev1.Secret
}

// settleFirewall decides where TrustGuard's firewall runs from --firewall
// (choice), NT_FIREWALL_JWT (jwt) and a trustguard-firewall Secret already in
// the namespace, without asking: in-cluster when chosen; hosted when chosen,
// or when a JWT or the Secret is there. It is unsettled when nothing decides,
// or when hosted is chosen with no JWT anywhere. A JWT from the environment
// or the Secret must be valid. install and preflight share it.
func settleFirewall(ctx context.Context, cs kubernetes.Interface, ns, choice, jwt string, replace bool) (settledFirewall, error) {
	if choice == firewallInCluster {
		return settledFirewall{settled: true, vals: values.InClusterFirewall()}, nil
	}
	if jwt != "" {
		if err := firewall.Validate(jwt); err != nil {
			return settledFirewall{}, fmt.Errorf("NT_FIREWALL_JWT: %w", err)
		}
		return settledFirewall{settled: true, vals: values.HostedFirewall(), secret: firewallSecret(ns, jwt)}, nil
	}
	keep, err := exists(ctx, cs, ns, values.FirewallJWTSecret, replace)
	if err != nil || !keep {
		return settledFirewall{}, err
	}
	if err := checkFirewallSecret(ctx, cs, ns); err != nil {
		return settledFirewall{}, err
	}
	return settledFirewall{settled: true, vals: values.HostedFirewall()}, nil
}

// checkFirewallSecret validates the FIREWALL_JWT in the trustguard-firewall
// Secret already in the namespace. A Secret ntctl may not read is left to
// TrustGuard to judge.
func checkFirewallSecret(ctx context.Context, cs kubernetes.Interface, ns string) error {
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, values.FirewallJWTSecret, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading Secret %s/%s: %w", ns, values.FirewallJWTSecret, err)
	}
	jwt, ok := s.Data[values.FirewallJWTKey]
	if !ok {
		return fmt.Errorf("the Secret %s/%s has no key %s: recreate it with the FIREWALL_JWT under that key", ns, values.FirewallJWTSecret, values.FirewallJWTKey)
	}
	if err := firewall.Validate(string(jwt)); err != nil {
		return fmt.Errorf("the Secret %s/%s: %w; replace it (NT_FIREWALL_JWT with --replace-secrets)", ns, values.FirewallJWTSecret, err)
	}
	return nil
}

func firewallSecret(ns, jwt string) *corev1.Secret {
	return secrets.Opaque(ns, values.FirewallJWTSecret, map[string]string{values.FirewallJWTKey: strings.TrimSpace(jwt)})
}

// describeFirewall says where TrustGuard's firewall runs, for the install
// summary, or "" without TrustGuard.
func describeFirewall(v string, vals map[string]any) string {
	if !has(values.Installed(vals), "trustguard") {
		return ""
	}
	deploy, set := values.Get(vals, "global", "firewall", "deploy").(bool)
	if url, _ := values.Get(vals, "global", "firewall", "baseURL").(string); url != "" && !deploy {
		return "yours, at " + url
	}
	if deploy || (!set && !values.HostedFirewallDefault(v)) {
		return "in this cluster"
	}
	name, _ := values.Get(vals, "trustguard", "firewall", "jwtExistingSecret", "name").(string)
	if name == "" {
		return "NeuralTrust-hosted"
	}
	return "NeuralTrust-hosted (FIREWALL_JWT in Secret " + name + ")"
}
