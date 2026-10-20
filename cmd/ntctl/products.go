package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// productChange is what an upgrade's --products and --remove-products change
// in a release: only products whose state differs from what it runs.
type productChange struct {
	add, remove []string
	// running are products named in --products that already run.
	running []string
}

// changeProducts works out the product change of an upgrade over current,
// the release's values with any -f files. Products named in --products are
// turned on and those in --remove-products off; every other product keeps its
// state, so a product left out of the list is never removed by accident.
func (o *opts) changeProducts(current map[string]any, remove string) (productChange, error) {
	var c productChange
	add, err := o.productList()
	if err != nil {
		return c, err
	}
	rm, err := parseProducts(remove)
	if err != nil {
		return c, fmt.Errorf("--remove-products: %w", err)
	}
	for _, p := range add {
		if has(rm, p) {
			return c, fmt.Errorf("%s is in both --products and --remove-products", p)
		}
	}
	installed := values.Installed(current)
	left := 0
	for _, p := range values.Products {
		switch {
		case has(add, p) && has(installed, p):
			c.running = append(c.running, p)
		case has(add, p):
			c.add = append(c.add, p)
		case has(rm, p) && has(installed, p):
			c.remove = append(c.remove, p)
			continue
		case !has(installed, p):
			continue
		}
		left++
	}
	if left == 0 && len(c.remove) > 0 {
		return c, fmt.Errorf("removing %s would leave the install with no product; keep at least one", strings.Join(c.remove, ", "))
	}
	return c, nil
}

func (c productChange) empty() bool { return len(c.add) == 0 && len(c.remove) == 0 }

// print says what the change does before anything is checked or asked.
func (c productChange) print(out io.Writer) {
	for _, p := range c.running {
		fmt.Fprintf(out, "%s is already installed.\n", p)
	}
	if len(c.add) > 0 {
		fmt.Fprintf(out, "Adding %s; the other products keep running as they are.\n", strings.Join(c.add, ", "))
	}
	if len(c.remove) > 0 {
		fmt.Fprintf(out, "Removing %s: the chart stops deploying it, and its workloads are deleted.\n", strings.Join(c.remove, ", "))
	}
}

// tokenSecrets settles the token Secrets these products need that are not in
// place yet. vals are the values the release will run with: a Secret they do
// not point a product at is not ntctl's to create. A token comes from its
// environment variable, else from the console when signed in (returned in
// need, and fetched only once the operator confirms), else from the operator.
// With placeholders nothing is asked and each Secret has an empty value, for a
// printed command or a dry run.
func tokenSecrets(ctx context.Context, in *prompt.Prompter, out io.Writer, cs kubernetes.Interface, ns string, sess *session, vals map[string]any, products []string, replace, placeholders bool) ([]*corev1.Secret, map[api.Product][]secrets.Spec, error) {
	var create []*corev1.Secret
	need := map[api.Product][]secrets.Spec{}
	// Only a hybrid data plane dials NeuralTrust with tokens.
	if values.DeploymentMode(vals) != "hybrid" {
		return nil, need, nil
	}
	for _, spec := range secrets.TokenSpecs(products) {
		if !referenced(vals, spec.Name) {
			continue
		}
		if keep, err := exists(ctx, cs, ns, spec.Name, replace); err != nil {
			return nil, nil, err
		} else if keep {
			// Said aloud: a Secret left by an earlier install may hold tokens
			// of another instance or console, which would not connect.
			fmt.Fprintf(out, "  Keeping the existing token Secret %s; --replace-secrets takes new tokens instead.\n", spec.Name)
			continue
		}
		if placeholders {
			create = append(create, secrets.Token(ns, spec, ""))
			continue
		}
		v := strings.TrimSpace(os.Getenv(spec.Env))
		if v == "" && sess != nil {
			need[api.Product(spec.Product)] = append(need[api.Product(spec.Product)], spec)
			continue
		}
		if v == "" {
			var err error
			if v, err = in.Secret(spec.Prompt + " (from the console)"); err != nil {
				return nil, nil, fmt.Errorf("%w; set %s, or sign in with `ntctl login`", err, spec.Env)
			}
		}
		create = append(create, secrets.Token(ns, spec, v))
	}
	return create, need, nil
}

// plannedKeys are the Secrets and keys a run will create, for the preflight
// Secrets check: those built already and those the console will issue.
func plannedKeys(create []*corev1.Secret, need map[api.Product][]secrets.Spec) map[string][]string {
	out := map[string][]string{}
	for _, specs := range need {
		for _, s := range specs {
			out[s.Name] = append(out[s.Name], s.Key)
		}
	}
	for _, s := range create {
		for k := range s.Data {
			out[s.Name] = append(out[s.Name], k)
		}
	}
	return out
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
