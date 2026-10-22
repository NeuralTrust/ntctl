package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/config"
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
	// cfg is what the console issued for it; nil until issue.
	cfg *api.InstallConfig
}

// consolePlan is where the tokens the console issues for a run come from: the
// organization and, per product, its private instance. It is empty when no
// token comes from the console.
type consolePlan struct {
	org, orgName string
	instances    []*consoleInstance
	// controlPlane is the control-plane domain the console's tokens are
	// expected to be valid at, guessed from its URL before anything is
	// issued; "" when the URL does not tell.
	controlPlane string
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
	if c.instances, err = chooseInstances(ctx, in, out, s, org, c.orgName, need, pick, defaultName); err != nil {
		return nil, err
	}
	c.controlPlane, _ = values.ControlPlaneFromConsole(s.cfg.AppURL)
	return c, nil
}

// issuedControlPlane is the control-plane domain the issued tokens are valid
// at, as the console reports it, or "" when it reports none that follows the
// chart's naming.
func (c *consolePlan) issuedControlPlane() (string, error) {
	var got, from string
	for _, i := range c.instances {
		if i.cfg == nil {
			continue
		}
		d, ok := values.ControlPlaneFromAddr(i.cfg.ConfigSyncEndpoint)
		if !ok {
			d, ok = values.ControlPlaneFromAddr(i.cfg.DataBridgeAddr)
		}
		if !ok {
			continue
		}
		if got != "" && got != d {
			return "", fmt.Errorf("the console issued %s's tokens for the control plane at %s and %s's for %s; one data plane dials one control plane", from, got, i.product.Name(), d)
		}
		got, from = d, i.product.Name()
	}
	return got, nil
}

// pointAtConsole settles which control plane a hybrid data plane dials when
// its tokens come from the console, which issues tokens only its own control
// plane accepts. vals are the values the release would run with. A new
// install is pointed at the console's control plane (the returned values);
// an existing release keeps the one it dials, so a different one is refused,
// as are values that name another control plane explicitly.
func pointAtConsole(vals map[string]any, domain string, fresh bool) (map[string]any, error) {
	if domain == "" || values.DeploymentMode(vals) != "hybrid" {
		return nil, nil
	}
	have, explicit := values.DialedControlPlane(vals)
	switch {
	case have == domain:
		return nil, nil
	case explicit:
		return nil, fmt.Errorf("your values point the data plane at the control plane %s (global.controlPlane.domain), but the console you are signed in to issues tokens for %s, which no other control plane accepts; "+
			"remove global.controlPlane.domain, or sign in to the console of %s", have, domain, have)
	case !fresh:
		return nil, fmt.Errorf("the release's data plane dials the control plane at %s, but the console you are signed in to issues tokens for %s, which no other control plane accepts; "+
			"sign in to the console of %s (`ntctl login`) to add products to this release", have, domain, have)
	}
	return values.ControlPlaneValues(domain), nil
}

// connects describes the control plane a hybrid release dials with vals, and
// when the operator is signed in to the console of another one, how to
// point it there. It never retargets the release itself: a sign-in to one
// console must not move a data plane whose tokens came from another. It
// returns "" for other modes.
func connects(vals map[string]any, appURL string) string {
	if values.DeploymentMode(vals) != "hybrid" {
		return ""
	}
	have, explicit := values.DialedControlPlane(vals)
	line := fmt.Sprintf("Connects to the control plane at %s.", have)
	console, ok := values.ControlPlaneFromConsole(appURL)
	if !ok || console == have || explicit {
		return line
	}
	return line + fmt.Sprintf(" You are signed in to the console of %s. If this install's tokens came from that console,\n"+
		"add --control-plane %s: no other control plane accepts them, and config sync stays Unauthenticated.", console, console)
}

// signedInConsole is the URL of the console ntctl is signed in to, or "".
func signedInConsole() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return cfg.AppURL
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
//
// An instance already named like the one the install would create (the
// domain, by default) is reused rather than duplicated, so running an install
// again after a failure keeps one instance. Reusing one is safe: the console
// issues new tokens for it, and those issued before keep working.
func chooseInstances(ctx context.Context, in *prompt.Prompter, out io.Writer, s *session, org, orgName string, need map[api.Product][]secrets.Spec, pick map[api.Product]string, defaultName string) ([]*consoleInstance, error) {
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
			if c.inst, err = findInstance(list, p, pick[p]); err != nil {
				return nil, err
			}
			if c.inst == nil {
				return nil, fmt.Errorf("no private %s %q in organization %s; see `ntctl %s list`", p.Name(), pick[p], orgName, strings.ToLower(p.Name()))
			}
		case in.Interactive && len(list) > 0:
			opts := make([]string, 0, len(list)+1)
			def := len(list)
			for i, inst := range list {
				opts = append(opts, fmt.Sprintf("Use %s (%s)", inst.Name, inst.ID))
				if strings.EqualFold(inst.Name, defaultName) && def == len(list) {
					def = i
				}
			}
			opts = append(opts, "Create a new private "+p.Name())
			choice, err := in.Choose("Which private "+p.Name()+" is this install?", opts, def)
			if err != nil {
				return nil, err
			}
			if choice < len(list) {
				c.inst = &list[choice]
				fmt.Fprintln(out, "  New tokens are issued for it; tokens issued before keep working.")
			}
		}
		if c.inst == nil {
			name := defaultName
			if in.Interactive {
				if name, err = in.Text("Name of the new private "+p.Name(), defaultName); err != nil {
					return nil, err
				}
			}
			if name = strings.TrimSpace(name); name == "" {
				return nil, fmt.Errorf("a name is needed for the new private %s", p.Name())
			}
			// A name already taken means that instance: install never
			// creates a second one under the same name.
			if c.inst, err = findInstance(list, p, name); err != nil {
				return nil, err
			}
			if c.inst != nil {
				fmt.Fprintf(out, "  Using the existing private %s %q (%s); new tokens are issued for it.\n", p.Name(), c.inst.Name, c.inst.ID)
			} else {
				c.newName = name
			}
		}
		plan = append(plan, c)
	}
	return plan, nil
}

// findInstance returns the instance whose ID is key or, failing that, the one
// whose name is key, ignoring case. It returns nil when none matches, and
// refuses a name that several instances share.
func findInstance(list []api.Instance, p api.Product, key string) (*api.Instance, error) {
	var named []int
	for i := range list {
		if list[i].ID == key {
			return &list[i], nil
		}
		if strings.EqualFold(list[i].Name, key) {
			named = append(named, i)
		}
	}
	switch len(named) {
	case 0:
		return nil, nil
	case 1:
		return &list[named[0]], nil
	}
	ids := make([]string, 0, len(named))
	for _, i := range named {
		ids = append(ids, list[i].ID)
	}
	return nil, fmt.Errorf("%d private %s instances are named %q (%s); name one by its ID with --%s-instance",
		len(named), p.Name(), key, strings.Join(ids, ", "), strings.ToLower(p.Name()))
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
	c.cfg = cfg
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
