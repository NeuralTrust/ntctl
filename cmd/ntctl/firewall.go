package main

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

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
// it open. Hosted needs the install's FIREWALL_JWT, from NT_FIREWALL_JWT, the
// terminal, or a trustguard-firewall Secret already in the namespace; ntctl
// stores a new one in that Secret. Without a terminal and without a JWT, the
// firewall runs in the cluster, as every 3.x install did. It returns the
// values to add and the Secret to create, either of which may be nil.
func firewallChoice(ctx context.Context, in *prompt.Prompter, cs kubernetes.Interface, ns string, f installFlags, v string, vals map[string]any) (map[string]any, *corev1.Secret, error) {
	if !values.NeedsFirewallChoice(v, vals) {
		return nil, nil, nil
	}
	jwt := os.Getenv("NT_FIREWALL_JWT")
	choice := f.firewall
	if choice == "" && jwt != "" {
		choice = firewallHosted
	}
	if choice == "" {
		i, err := in.Choose("Where should TrustGuard's firewall run?", []string{
			"In this cluster",
			"The NeuralTrust-hosted firewall, with this install's FIREWALL_JWT",
		}, 0)
		if err != nil {
			return nil, nil, err
		}
		choice = firewallInCluster
		if i == 1 {
			choice = firewallHosted
		}
	}
	if choice == firewallInCluster {
		return values.InClusterFirewall(), nil, nil
	}
	if jwt == "" {
		keep, err := exists(ctx, cs, ns, values.FirewallJWTSecret, f.replace)
		if err != nil {
			return nil, nil, err
		}
		if keep {
			return values.HostedFirewall(), nil, nil
		}
		if jwt, err = in.Secret("FIREWALL_JWT (shown by the console when it created the TrustGuard, or sent by NeuralTrust)"); err != nil {
			return nil, nil, fmt.Errorf("the hosted firewall needs this install's FIREWALL_JWT: set NT_FIREWALL_JWT, or pass --firewall %s: %w", firewallInCluster, err)
		}
	}
	return values.HostedFirewall(), secrets.Opaque(ns, values.FirewallJWTSecret, map[string]string{values.FirewallJWTKey: jwt}), nil
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
