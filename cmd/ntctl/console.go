package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// consoleInstance is a private instance an install takes its tokens from,
// when signed in to the console.
type consoleInstance struct {
	product api.Product
	// specs are the token Secrets still to create for it.
	specs []secrets.Spec
	// inst is the instance; nil until a new one named newName is created.
	inst    *api.Instance
	newName string
}

// consolePlan is where the tokens the console issues for a run come from: the
// organization and, per product, its private instance. It is empty when no
// token comes from the console.
type consolePlan struct {
	org, orgName string
	instances    []*consoleInstance
}

// planConsole settles the consolePlan for the products in need. Nothing is
// created: that waits until the operator confirms.
func planConsole(ctx context.Context, in *prompt.Prompter, out io.Writer, s *session, orgFlag string, need map[api.Product][]secrets.Spec, pick map[api.Product]string, defaultName string) (*consolePlan, error) {
	c := &consolePlan{}
	if len(need) == 0 {
		return c, nil
	}
	org, err := s.org(orgFlag)
	if err != nil {
		return nil, err
	}
	c.org, c.orgName = org, org
	if org == s.cfg.OrgID && s.cfg.OrgName != "" {
		c.orgName = s.cfg.OrgName
	}
	if c.instances, err = chooseInstances(ctx, in, out, s, org, need, pick, defaultName); err != nil {
		return nil, err
	}
	return c, nil
}

// describe prints a summary line per instance.
func (c *consolePlan) describe(out io.Writer) {
	for _, i := range c.instances {
		fmt.Fprintf(out, "  %s\n", i.describe(c.orgName))
	}
}

// issue creates the new instances and returns the token Secrets to create.
func (c *consolePlan) issue(ctx context.Context, out io.Writer, s *session, ns string) ([]*corev1.Secret, error) {
	var list []*corev1.Secret
	for _, i := range c.instances {
		l, err := i.issue(ctx, out, s, c.org, ns)
		if err != nil {
			return nil, err
		}
		list = append(list, l...)
	}
	return list, nil
}

// finalize records each instance's endpoint in the console.
func (c *consolePlan) finalize(ctx context.Context, out io.Writer, s *session, vals map[string]any) {
	for _, i := range c.instances {
		i.finalize(ctx, out, s, c.org, vals)
	}
}

// chooseInstances settles, per product that still needs tokens, which private
// instance they come from. Nothing is created yet: that waits until the
// operator confirms the install.
func chooseInstances(ctx context.Context, in *prompt.Prompter, out io.Writer, s *session, org string, need map[api.Product][]secrets.Spec, pick map[api.Product]string, defaultName string) ([]*consoleInstance, error) {
	var plan []*consoleInstance
	for _, p := range []api.Product{api.TrustGate, api.TrustGuard} {
		specs := need[p]
		if len(specs) == 0 {
			continue
		}
		c := &consoleInstance{product: p, specs: specs}
		list, err := s.api.Instances(ctx, p, org)
		if err != nil {
			return nil, fmt.Errorf("listing private %s instances: %w", p.Name(), err)
		}
		switch {
		case pick[p] != "":
			for i := range list {
				if list[i].ID == pick[p] || strings.EqualFold(list[i].Name, pick[p]) {
					c.inst = &list[i]
				}
			}
			if c.inst == nil {
				return nil, fmt.Errorf("no private %s %q in organization %s; see `ntctl %s list`", p.Name(), pick[p], org, strings.ToLower(p.Name()))
			}
		case in.Interactive && len(list) > 0:
			opts := make([]string, 0, len(list)+1)
			for _, i := range list {
				opts = append(opts, fmt.Sprintf("Use %s (%s)", i.Name, i.ID))
			}
			opts = append(opts, "Create a new private "+p.Name())
			choice, err := in.Choose("Which private "+p.Name()+" is this install?", opts, len(opts)-1)
			if err != nil {
				return nil, err
			}
			if choice < len(list) {
				c.inst = &list[choice]
				fmt.Fprintln(out, "  New tokens are issued for it; tokens issued before keep working.")
			}
		}
		if c.inst == nil {
			if c.newName, err = in.Text("Name of the new private "+p.Name(), defaultName); err != nil {
				return nil, err
			}
			if strings.TrimSpace(c.newName) == "" {
				return nil, fmt.Errorf("a name is needed for the new private %s", p.Name())
			}
		}
		plan = append(plan, c)
	}
	return plan, nil
}

// describe is the install summary line for the instance.
func (c *consoleInstance) describe(orgName string) string {
	what := fmt.Sprintf("new private instance %q", c.newName)
	if c.inst != nil {
		what = fmt.Sprintf("private instance %q (%s)", c.inst.Name, c.inst.ID)
	}
	return fmt.Sprintf("%-11s %s in %s; tokens from the console", c.product.Name()+":", what, orgName)
}

// issue creates the instance when it is new, then fetches its tokens and
// returns them as the Secrets to create.
func (c *consoleInstance) issue(ctx context.Context, out io.Writer, s *session, org, ns string) ([]*corev1.Secret, error) {
	if c.inst == nil {
		inst, err := s.api.Create(ctx, c.product, org, c.newName)
		if err != nil {
			return nil, fmt.Errorf("creating the private %s: %w", c.product.Name(), err)
		}
		c.inst = inst
		fmt.Fprintf(out, "Created private %s %q (%s)\n", c.product.Name(), inst.Name, inst.ID)
	}
	cfg, err := s.api.InstallConfig(ctx, c.product, org, c.inst.ID, false)
	if err != nil {
		return nil, fmt.Errorf("fetching the tokens of %s %s: %w", c.product.Name(), c.inst.ID, err)
	}
	var list []*corev1.Secret
	for _, spec := range c.specs {
		v := cfg.ConfigSyncToken
		if spec.Key == values.EnrolmentKey {
			v = cfg.EnrolmentToken
		}
		list = append(list, secrets.Token(ns, spec, v))
	}
	return list, nil
}

// finalize records the installed instance's endpoint in the console. A
// failure is reported, not returned: the install itself succeeded.
func (c *consoleInstance) finalize(ctx context.Context, out io.Writer, s *session, org string, vals map[string]any) {
	ep := endpoint(vals, c.product)
	if ep == "" || c.inst == nil {
		return
	}
	if err := s.api.Finalize(ctx, c.product, org, c.inst.ID, ep); err != nil {
		fmt.Fprintf(out, "Could not record %s's endpoint %s in the console (%v); set it there.\n", c.inst.Name, ep, err)
		return
	}
	fmt.Fprintf(out, "Recorded %s endpoint %s in the console.\n", c.inst.Name, ep)
}

// endpoint is the public URL the chart gives the product's data plane:
// <ingress.dataPlane.host>, else <hostPrefix>.<global.domain>.
func endpoint(vals map[string]any, p api.Product) string {
	root, prefix := "agentgateway", "gateway"
	if p == api.TrustGuard {
		root, prefix = "trustguard", "trustguard"
	}
	host, _ := values.Get(vals, root, "ingress", "dataPlane", "host").(string)
	if host == "" {
		domain, _ := values.Get(vals, "global", "domain").(string)
		if domain == "" {
			return ""
		}
		if v, _ := values.Get(vals, root, "ingress", "dataPlane", "hostPrefix").(string); v != "" {
			prefix = v
		}
		host = prefix + "." + domain
	}
	return "https://" + host
}
