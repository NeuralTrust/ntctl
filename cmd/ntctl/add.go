package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/redact"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// addProducts is `ntctl install` where the release already exists: instead of
// refusing, it adds products to it. Every other setting comes from the
// release, so it asks only which products to add and their tokens, then
// upgrades in place on the installed chart, as `ntctl upgrade --products`
// does. Running it again with nothing new changes nothing.
func (o *opts) addProducts(ctx context.Context, in *prompt.Prompter, out io.Writer, f installFlags, sess *session, rel *release.Release) error {
	if f.print() {
		return fmt.Errorf("release %q already exists in namespace %q, and --secrets-mode print is for a new install; add a product with `ntctl install --products <product>`, or create its Secrets yourself and run `ntctl upgrade -f <values>`", o.release, o.kube.Namespace)
	}
	files, err := values.Files(o.kube.Settings(), o.valueFiles)
	if err != nil {
		return err
	}
	base := reuse(rel.Config, files)
	installed := values.Installed(base)
	fmt.Fprintf(out, "Release %q is already installed in namespace %q: chart %s, products %s.\n"+
		"Adding products keeps every other setting of the release; change those with `ntctl upgrade`.\n\n",
		o.release, o.kube.Namespace, rel.Chart.Metadata.Version, list(installed))
	if err := o.sameAsRelease(out, f, base); err != nil {
		return err
	}

	var missing []string
	for _, p := range values.Products {
		if !has(installed, p) {
			missing = append(missing, p)
		}
	}
	if o.products == "" {
		if len(missing) == 0 {
			fmt.Fprintln(out, "Every product is already installed; nothing to do.")
			return nil
		}
		// Without a terminal, only --products adds: a repeated install
		// script must not add products nobody chose.
		if !in.Interactive {
			fmt.Fprintf(out, "Nothing to add; pass --products to add %s.\n", list(missing))
			return nil
		}
		for {
			if o.products, err = in.Text("Products to add, comma-separated", strings.Join(missing, ",")); err != nil {
				return err
			}
			if _, err = o.productList(); err == nil {
				break
			}
			fmt.Fprintf(out, "  %v\n", err)
		}
	}
	add, err := o.productList()
	if err != nil {
		return err
	}
	if err := configSyncFits(base, append(installed, add...)); err != nil {
		return err
	}
	// Adding a product never also moves the chart.
	if o.version == "" && o.chartPath == "" {
		o.version = rel.Chart.Metadata.Version
	}

	plan, err := o.planChange(ctx, in, out, rel, "", sess, f.org, f.instances, f.replace, false)
	if err != nil {
		return err
	}
	if plan.change.empty() {
		fmt.Fprintln(out, "Every product you chose is already installed; nothing to do.")
		return nil
	}
	vals, err := o.applyChange(ctx, in, out, changeFlags{yes: f.yes, timeout: f.timeout}, plan)
	if err != nil || vals == nil {
		return err
	}
	if err := updateSavedValues(out, f.saveValues, vals); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nAdded %s. Create any new DNS records above, then finish the setup in the NeuralTrust console.\n", list(plan.change.add))
	return nil
}

// sameAsRelease refuses install flags that would change a setting of the
// release, so that re-running the first install's command to add a product
// cannot change anything else. Flags that only create something the release
// already has are reported and ignored.
func (o *opts) sameAsRelease(out io.Writer, f installFlags, current map[string]any) error {
	differs := func(flag, value, path string) error {
		have, _ := values.Get(current, strings.Split(path, ".")...).(string)
		if value == "" || value == have {
			return nil
		}
		return fmt.Errorf("--%s %s differs from the release's %s (%q). Adding a product keeps the release's settings; change it with `ntctl upgrade --%s`, then add the product", flag, value, path, have, flag)
	}
	if err := differs("domain", o.domain, "global.domain"); err != nil {
		return err
	}
	if err := differs("platform", o.platform, "global.platform"); err != nil {
		return err
	}
	if err := differs("image-registry", f.imageRegistry, "global.imageRegistry"); err != nil {
		return err
	}
	if o.gateway != "" {
		ref, err := gatewayapi.ParseRef(o.gateway)
		if err != nil {
			return err
		}
		if refs := gatewayRefs(current); len(refs) != 1 || refs[0] != ref.String() {
			return fmt.Errorf("--gateway %s differs from how the release is exposed (%s). Adding a product keeps the release's settings; change it with `ntctl upgrade --gateway`, then add the product", ref, orIngress(exposure(current)))
		}
	}
	if f.registryKey != "" {
		fmt.Fprintln(out, "  The registry key is not used: the release keeps its image pull settings.")
	}
	return nil
}

// configSyncFits refuses a TrustGate and TrustGuard pair behind one
// global.controlPlane.configSyncAddr, which the chart cannot render: each
// product has its own config-sync listener and certificate.
func configSyncFits(current map[string]any, products []string) error {
	addr, _ := values.Get(current, "global", "controlPlane", "configSyncAddr").(string)
	if strings.TrimSpace(addr) == "" || !has(products, "trustgate") || !has(products, "trustguard") {
		return nil
	}
	return errors.New("the release dials config sync at one address, global.controlPlane.configSyncAddr, which serves a single product; " +
		"TrustGate and TrustGuard each need their own. Unset it and set agentgateway.configSync.endpoint and trustguard.configSync.endpoint " +
		"(or rely on DNS, <product>-configsync.<domain>:443) with `ntctl upgrade -f`, then add the product")
}

// updateSavedValues rewrites the values file the install saved, when it is
// still there, so it keeps matching the release. Anything that looks like a
// secret is left out, as adopt does.
func updateSavedValues(out io.Writer, path string, vals map[string]any) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	clean, _ := redact.Value(deepCopy(vals)).(map[string]any)
	if err := values.Save(path, clean); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated the values in %s (no credentials).\n", path)
	return nil
}

func list(products []string) string {
	if len(products) == 0 {
		return "none"
	}
	return strings.Join(products, ", ")
}

func orIngress(exposure string) string {
	if exposure == "" {
		return "Ingresses"
	}
	return exposure
}
