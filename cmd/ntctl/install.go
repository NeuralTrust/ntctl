package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/auth"
	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/report"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/setup"
	"github.com/NeuralTrust/ntctl/internal/values"
)

const installSummary = `Install the NeuralTrust platform. On a terminal ntctl asks for anything the
flags and -f files leave open (where the control plane runs, domain, products,
operational telemetry, where images come from, your own PostgreSQL or Redis),
each with a default. It then checks the cluster, creates the Secrets,
installs, waits until ready and prints the DNS records.

--mode says where the control plane runs: hybrid (the default) installs a data
plane that NeuralTrust's control plane manages; external installs everything
in this cluster and nothing leaves it; saas installs a control plane here that
serves data planes elsewhere. External and saas installs ask for the console's
first admin, from ONPREM_SUPERADMIN_EMAIL and ONPREM_SUPERADMIN_PASSWORD or the
terminal, and need no NeuralTrust token.

When the release already exists, install adds products to it instead: it
asks only which products to add and their tokens, keeps every other setting
of the release and its chart version, and upgrades in place after showing
what changes. With nothing new to add it changes nothing.

Operational telemetry is off unless you turn it on, here or later with
ntctl upgrade --operational-telemetry=true --version <installed version>. TrustGate and TrustGuard then
export their own traces and metrics (request rate, errors, latency): in hybrid
installs to NeuralTrust, to help support diagnose your gateway; otherwise to
your in-cluster collector. We recommend it for faster support.

Credentials are read from the environment, or asked for with input hidden:
  NT_TRUSTGATE_CONFIG_SYNC_TOKEN   NT_TRUSTGATE_ENROLMENT_TOKEN
  NT_TRUSTGUARD_CONFIG_SYNC_TOKEN  NT_TRUSTGUARD_ENROLMENT_TOKEN
  NT_POSTGRES_URL   postgres://user:password@host:5432/database?sslmode=require
  NT_REDIS_URL      redis://:password@host:6379 (rediss:// for TLS)
  NT_REGISTRY_USERNAME / NT_REGISTRY_PASSWORD   for --image-registry
They are written only to Kubernetes Secrets in the install namespace.

Signed in with ntctl login, the TrustGate and TrustGuard tokens come from the
console instead: install creates a private instance for each product, or uses
the one named with --trustgate-instance / --trustguard-instance.

With --secrets-mode print, ntctl asks and checks as usual but changes nothing:
it saves the values and prints the kubectl commands that create the Secrets,
with <…> placeholders for every credential. Run them yourself, then run
install again with -f and the saved values file to install.`

var platforms = []string{"aws", "gcp", "azure", "openshift", "kubernetes"}

// installFlags are the install-only flags.
type installFlags struct {
	registryKey   string
	imageRegistry string
	nodeIdentity  bool
	saveValues    string
	replace       bool
	secretsMode   string
	yes           bool
	timeout       time.Duration
	org           string
	instances     map[api.Product]string
}

func runInstall(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	var o opts
	f := installFlags{instances: map[api.Product]string{}}
	var gate, guard string
	fs := newFlags("install", installSummary, out, &o, true)
	fs.StringVar(&f.registryKey, "registry-key", os.Getenv("NT_REGISTRY_KEY_FILE"), "registry key file from NeuralTrust (env NT_REGISTRY_KEY_FILE)")
	fs.StringVar(&f.imageRegistry, "image-registry", "", "pull every image from this registry instead, e.g. registry.example.com/neuraltrust")
	fs.BoolVar(&f.nodeIdentity, "image-registry-node-auth", false, "nodes authenticate to --image-registry with their own identity; no pull Secret")
	fs.StringVar(&f.saveValues, "save-values", "neuraltrust-values.yaml", "write the values used, without credentials, to this file (empty to skip)")
	fs.BoolVar(&f.replace, "replace-secrets", false, "overwrite Secrets that already exist with different content")
	fs.StringVar(&f.secretsMode, "secrets-mode", "kubernetes", "kubernetes: create the Secrets; print: print the kubectl commands that create them, install nothing")
	fs.BoolVar(&f.yes, "yes", false, "ask nothing: take defaults and do not ask for confirmation")
	fs.DurationVar(&f.timeout, "timeout", helm.DefaultTimeout, "how long to wait for the install to become ready")
	fs.StringVar(&f.org, "org", "", "signed in: organization ID (default: the selected one)")
	fs.StringVar(&gate, "trustgate-instance", "", "signed in: ID or name of the private TrustGate to install (default: create one)")
	fs.StringVar(&guard, "trustguard-instance", "", "signed in: ID or name of the private TrustGuard to install (default: create one)")
	if err := parse(fs, args); err != nil {
		return err
	}
	f.instances[api.TrustGate], f.instances[api.TrustGuard] = gate, guard
	switch {
	case f.secretsMode != "kubernetes" && f.secretsMode != "print":
		return fmt.Errorf("--secrets-mode must be kubernetes or print, not %q", f.secretsMode)
	case f.print() && f.saveValues == "":
		return errors.New("--secrets-mode print needs --save-values: the second run installs from that file")
	case f.print() && f.replace:
		return errors.New("--secrets-mode print changes no Secret; replace one yourself instead of passing --replace-secrets")
	}
	in := prompt.New(ctx, stdin, out, !f.yes)
	sess, err := openSession()
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		sess = nil
	case err != nil:
		return err
	}
	ns := o.kube.Namespace

	cs, dyn, err := o.clients()
	if err != nil {
		return err
	}
	cluster, err := detect.Inspect(ctx, cs, dyn)
	if err != nil {
		return fmt.Errorf("cannot use the cluster from your kubeconfig: %w", err)
	}
	fmt.Fprintf(out, "Cluster: %s, %s, platform %s\n\n", o.kube.Target(), cluster.Version, cluster.Platform)

	// An existing release is found before anything is asked: installing
	// again adds products to it.
	cfg, err := o.kube.Helm(action.ConfigurationSetLogger(helm.Logger()))
	if err != nil {
		return err
	}
	if rel, err := helm.Current(cfg, o.release); err != nil {
		return err
	} else if rel != nil {
		return o.addProducts(ctx, in, out, f, sess, rel)
	}

	files, err := values.Files(o.kube.Settings(), o.valueFiles)
	if err != nil {
		return err
	}
	answers, err := ask(ctx, in, out, cs, cluster, &o, f, files)
	if err != nil {
		return err
	}
	o.extra = answers.Values()
	// The chart reaches the console's region; signed in, that is known.
	if sess != nil && sess.cfg.Region != "" && values.Get(files, "global", "saasRegion") == nil {
		o.extra = values.Merge(o.extra, map[string]any{"global": map[string]any{"saasRegion": sess.cfg.Region}})
	}

	// Everything to create, gathered before anything changes.
	create, err := answers.Secrets(ns)
	if err != nil {
		return err
	}
	vals, err := o.effectiveValues(installDefaults(cluster.Platform))
	if err != nil {
		return err
	}
	// Tokens come from the environment, else from the console when signed
	// in, else from the operator. Printed, they are placeholders.
	tokens, need, err := tokenSecrets(ctx, in, cs, ns, sess, vals, enabledProducts(vals), f.replace, f.print())
	if err != nil {
		return err
	}
	create = append(create, tokens...)
	consoles, err := planConsole(ctx, in, out, sess, f.org, need, f.instances, instanceName(vals, o.release))
	if err != nil {
		return err
	}
	// The console's tokens are valid only at its own control plane, which
	// is not always the region's (a dev or self-hosted console).
	if cp, err := pointAtConsole(vals, consoles.controlPlane, true); err != nil {
		return err
	} else if cp != nil {
		o.extra = values.Merge(orEmpty(o.extra), cp)
	}

	fmt.Fprintln(out, "\nChecking the cluster against this install…")
	p, err := o.prepare(ctx, preflight.ForInstall, plannedKeys(create, need))
	if err != nil {
		return err
	}
	if err := preflight.Print(out, p.results, false); err != nil {
		return err
	}
	if preflight.Failed(p.results) {
		return errPreflight
	}
	if f.print() {
		return printSecrets(ctx, out, p.cs, ns, create, answers.Images.KeyFile, f.saveValues, p.values)
	}

	fmt.Fprintf(out, "\nReady to install chart %s %s as release %q in namespace %q\non %s.\n",
		p.chart.Name, p.chart.Version, o.release, ns, o.kube.Target())
	domain, _ := values.Get(p.values, "global", "domain").(string)
	platform, _ := values.Get(p.values, "global", "platform").(string)
	fmt.Fprintf(out, "  Mode:       %s\n", describeMode(values.DeploymentMode(p.values)))
	fmt.Fprintf(out, "  Domain:     %s\n  Platform:   %s\n  Products:   %s\n", domain, platform, strings.Join(values.Installed(p.values), ", "))
	if line := exposure(p.values); line != "" {
		fmt.Fprintf(out, "  Endpoints:  %s\n", line)
	}
	fmt.Fprintf(out, "  Telemetry:  operational traces and metrics %s\n", values.TelemetryState(p.values))
	if cp, _ := values.DialedControlPlane(p.values); cp != "" && values.DeploymentMode(p.values) == "hybrid" {
		fmt.Fprintf(out, "  Connects:   control plane at %s\n", cp)
	}
	for _, line := range answers.Summary() {
		fmt.Fprintf(out, "  %s\n", line)
	}
	consoles.describe(out)
	if !f.yes {
		if err := in.Confirm("Continue?"); err != nil {
			return err
		}
	}

	if created, err := secrets.Namespace(ctx, p.cs, ns); err != nil {
		return err
	} else if created {
		fmt.Fprintf(out, "Created namespace %s\n", ns)
	}
	issued, err := consoles.issue(ctx, out, sess, ns)
	if err != nil {
		return err
	}
	create = append(create, issued...)
	// The console says where its tokens are valid; that wins over the guess
	// made from its URL.
	if d, err := consoles.issuedControlPlane(); err != nil {
		return err
	} else if d != "" && values.DeploymentMode(p.values) == "hybrid" {
		if _, err := pointAtConsole(vals, d, true); err != nil {
			return err
		}
		if have, _ := values.DialedControlPlane(p.values); have != d {
			values.Merge(p.values, values.ControlPlaneValues(d))
			fmt.Fprintf(out, "The console issued the tokens for the control plane at %s; the data plane dials it.\n", d)
		}
	}
	done, err := secrets.Apply(ctx, p.cs, create, f.replace)
	for _, d := range done {
		fmt.Fprintf(out, "Secret: %s\n", d)
	}
	if err != nil {
		return err
	}
	if f.saveValues != "" {
		if err := values.Save(f.saveValues, p.values); err != nil {
			return err
		}
		fmt.Fprintf(out, "Saved the values to %s (no credentials).\n", f.saveValues)
	}

	fmt.Fprintf(out, "Installing… waiting up to %s for every workload to become ready.\n", f.timeout)
	_, installErr := helm.Install(ctx, p.cfg, p.chart, p.values, o.release, ns, f.timeout)
	if st, err := report.Collect(ctx, p.cs, p.dyn, ns); err == nil {
		fmt.Fprintln(out)
		st.Print(out)
	}
	if installErr != nil {
		return fmt.Errorf("install did not complete: %w\nThe resources were kept so you can see what failed: %s", installErr, fixThenUpgrade)
	}
	fmt.Fprintln(out)
	consoles.finalize(ctx, out, sess, p.values)
	fmt.Fprintln(out, installedMessage(p.values))
	return nil
}

// installedMessage is the last line of a successful install: where the setup
// continues, which depends on where the control plane runs.
func installedMessage(vals map[string]any) string {
	if values.DeploymentMode(vals) == "hybrid" {
		return "Installed. Create the DNS records above, then finish the setup in the NeuralTrust console."
	}
	domain, _ := values.Get(vals, "global", "domain").(string)
	msg := fmt.Sprintf("Installed. Create the DNS records above, then sign in to this install's console at https://app.%s.", domain)
	if values.DeploymentMode(vals) == "saas" {
		cp, _ := values.Get(vals, "global", "controlPlane", "domain").(string)
		if cp == "" {
			cp = domain
		}
		msg += fmt.Sprintf(`
To connect a data plane that runs elsewhere:
  1. Export this control plane's CA with the chart's scripts/export-controlplane-ca.sh,
     and apply the bundle in the data plane's namespace (Secret controlplane-ca).
  2. Create a private gateway in this console, for the data plane's tokens.
  3. Install the data plane with values setting global.controlPlane.domain: %s
     and global.controlPlane.caSecretName: controlplane-ca, and with that gateway's
     tokens in NT_TRUSTGATE_CONFIG_SYNC_TOKEN and NT_TRUSTGATE_ENROLMENT_TOKEN.`, cp)
	}
	return msg
}

// describeMode says in words where the control plane of a mode runs.
func describeMode(mode string) string {
	switch mode {
	case "external":
		return "external (everything runs in this cluster; nothing leaves it)"
	case "saas":
		return "saas (this cluster is a control plane for data planes elsewhere)"
	}
	return mode + " (NeuralTrust hosts the control plane; this cluster runs the data plane)"
}

// sameMode refuses a --mode other than the release's: where the control
// plane runs cannot change in place.
func (o *opts) sameMode(current map[string]any) error {
	if _, err := o.effectiveMode(nil); err != nil || o.mode == "" {
		return err
	}
	if have := values.DeploymentMode(current); o.mode != have {
		return fmt.Errorf("--mode %s differs from the release's mode, %s; a release cannot change where its control plane runs in place. Install the other mode as a separate release (--namespace)", o.mode, have)
	}
	return nil
}

// orEmpty returns m, or an empty map when m is nil, so it can be merged into.
func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// ask settles every install choice the flags and files leave open. On a
// terminal it asks, offering a default for each; otherwise it takes the
// defaults and the environment.
func ask(ctx context.Context, in *prompt.Prompter, out io.Writer, cs kubernetes.Interface, cluster *detect.Cluster, o *opts, f installFlags, files map[string]any) (setup.Answers, error) {
	var a setup.Answers
	set := func(path ...string) bool { return values.Get(files, path...) != nil }

	mode, err := o.effectiveMode(files)
	if err != nil {
		return a, err
	}
	if mode == "" && in.Interactive {
		i, err := in.Choose("Where does the control plane run?", []string{
			"NeuralTrust hosts it; this cluster runs the data plane (hybrid)",
			"In this cluster, with everything else; nothing leaves it (external)",
			"In this cluster, serving data planes that run elsewhere (central control plane, saas)",
		}, 0)
		if err != nil {
			return a, err
		}
		// Hybrid is the chart's default: choosing it writes nothing.
		if i > 0 {
			o.mode = values.Modes[i]
		}
		mode = values.Modes[i]
	}
	if mode == "" {
		mode = "hybrid"
	}

	if o.domain == "" && !set("global", "domain") {
		if o.domain, err = in.Text("Base domain for the endpoints, e.g. platform.example.com", ""); err != nil {
			return a, err
		}
	}
	if o.platform == "" && !set("global", "platform") && in.Interactive {
		i, err := in.Choose("Platform (detected: "+cluster.Platform+")", platforms, index(platforms, cluster.Platform))
		if err != nil {
			return a, err
		}
		o.platform = platforms[i]
	}
	if o.gateway == "" && !set("global", "ingress", "provider") && in.Interactive && len(cluster.Gateways) > 0 {
		platform := o.platform
		if platform == "" {
			platform = cluster.Platform
		}
		if o.gateway, err = askExposure(in, cluster, platform); err != nil {
			return a, err
		}
	}
	if o.products == "" && !set("global", "products") && in.Interactive {
		for {
			if o.products, err = in.Text("Products, comma-separated", strings.Join(values.Products, ",")); err != nil {
				return a, err
			}
			if _, err = o.productList(); err == nil {
				break
			}
			fmt.Fprintf(out, "  %v\n", err)
		}
	}
	if o.telemetry.v == nil && !set("global", "observability", "operationalTelemetry") && in.Interactive {
		on, err := askTelemetry(in, out, mode)
		if err != nil {
			return a, err
		}
		o.telemetry.v = &on
	}

	// Images.
	switch {
	case f.imageRegistry != "":
		a.Images = setup.Images{Registry: f.imageRegistry, NodeIdentity: f.nodeIdentity,
			Username: os.Getenv("NT_REGISTRY_USERNAME"), Password: os.Getenv("NT_REGISTRY_PASSWORD")}
	case set("global", "imageRegistry"):
		// The values file names the registry; its pull Secret is the operator's.
	default:
		choice := 0
		if f.registryKey == "" {
			if choice, err = in.Choose("Where should the cluster pull images from?", []string{
				"The NeuralTrust registry, with the key file NeuralTrust sent you",
				"My own registry, where the images are mirrored",
			}, 0); err != nil {
				return a, err
			}
		}
		if choice == 1 {
			if a.Images, err = askMirror(in, f.print()); err != nil {
				return a, err
			}
			break
		}
		keep, err := exists(ctx, cs, o.kube.Namespace, secrets.PullSecret, f.replace)
		if err != nil {
			return a, err
		}
		if keep && f.registryKey == "" {
			fmt.Fprintf(out, "  Using the existing pull Secret %s.\n", secrets.PullSecret)
			break
		}
		path := f.registryKey
		if path == "" {
			def := ""
			if _, err := os.Stat("registry-key.json"); err == nil {
				def = "registry-key.json"
			}
			if path, err = in.Text("Path to the registry key file", def); err != nil {
				return a, err
			}
		}
		a.Images.KeyFile = path
		if a.Images.RegistryKey, err = os.ReadFile(path); err != nil { // #nosec G304 -- path chosen by the operator
			return a, fmt.Errorf("reading registry key: %w", err)
		}
	}

	// Datastores. Hybrid shares one PostgreSQL role and one Redis, which is
	// what these questions configure. The other modes give every service its
	// own database role; that is set with -f.
	if mode != "hybrid" {
		if !set("global", "postgresql") || !set("global", "redis") {
			fmt.Fprintf(out, "  Mode %s: PostgreSQL and Redis run in the cluster unless your -f values configure them (see values-managed-datastores.yaml.example in the chart).\n", mode)
		}
		if !set("global", "superadmin", "existingSecret", "name") && !set("global", "superadmin", "email") {
			if a.Admin, err = askAdmin(ctx, in, out, cs, o.kube.Namespace, f); err != nil {
				return a, err
			}
		}
		return a, nil
	}
	if !set("global", "postgresql") {
		question, parse := "Connection string, postgres://user:password@host:5432/database?sslmode=require", setup.ParsePostgres
		if f.print() {
			question, parse = "Connection string, postgres://user@host:5432/database?sslmode=require (the password goes in the printed command)", setup.ParsePostgresAddress
		}
		if a.Postgres, err = askDatastore(in, out, "PostgreSQL", "NT_POSTGRES_URL", question, parse); err != nil {
			return a, err
		}
		if a.Postgres != nil {
			fmt.Fprintln(out, "  The user and database must already exist; NeuralTrust creates its tables on first start.")
		}
	}
	if !set("global", "redis") {
		if a.Redis, err = askDatastore(in, out, "Redis", "NT_REDIS_URL",
			"Redis URL, redis://:password@host:6379 (rediss:// for TLS)",
			setup.ParseRedis); err != nil {
			return a, err
		}
	}
	return a, nil
}

// minAdminPassword is the shortest first-admin password install accepts.
const minAdminPassword = 12

// askAdmin settles the console's first admin of an external or saas install:
// an existing admin Secret is kept, else the login comes from
// ONPREM_SUPERADMIN_EMAIL and ONPREM_SUPERADMIN_PASSWORD, else from the
// operator. With --secrets-mode print the password is left to the printed
// command.
func askAdmin(ctx context.Context, in *prompt.Prompter, out io.Writer, cs kubernetes.Interface, ns string, f installFlags) (*setup.Admin, error) {
	keep, err := exists(ctx, cs, ns, setup.AdminSecret, f.replace)
	if err != nil {
		return nil, err
	}
	if keep {
		fmt.Fprintf(out, "  Using the existing admin Secret %s.\n", setup.AdminSecret)
		return &setup.Admin{Existing: true}, nil
	}
	ad := &setup.Admin{Email: strings.TrimSpace(os.Getenv(setup.AdminEmailKey)), Password: os.Getenv(setup.AdminPassKey)}
	missing := fmt.Errorf("the console's first admin is needed: set %s and %s, or point global.superadmin.existingSecret.name at a Secret of yours", setup.AdminEmailKey, setup.AdminPassKey)
	if ad.Email == "" {
		if !in.Interactive {
			return nil, missing
		}
		for {
			if ad.Email, err = in.Text("Email of the console's first admin", ""); err != nil {
				return nil, err
			}
			if ad.Email = strings.TrimSpace(ad.Email); strings.Contains(ad.Email, "@") {
				break
			}
			fmt.Fprintln(out, "  Enter an email address.")
		}
	}
	if f.print() {
		ad.Password = ""
		return ad, nil
	}
	if ad.Password != "" {
		if len(ad.Password) < minAdminPassword {
			return nil, fmt.Errorf("%s must be at least %d characters", setup.AdminPassKey, minAdminPassword)
		}
		return ad, nil
	}
	if !in.Interactive {
		return nil, missing
	}
	for {
		if ad.Password, err = in.Secret(fmt.Sprintf("Password for %s (at least %d characters)", ad.Email, minAdminPassword)); err != nil {
			return nil, err
		}
		again, err := in.Secret("Repeat the password")
		if err != nil {
			return nil, err
		}
		switch {
		case len(ad.Password) < minAdminPassword:
			fmt.Fprintf(out, "  The password needs at least %d characters.\n", minAdminPassword)
		case again != ad.Password:
			fmt.Fprintln(out, "  The passwords differ.")
		default:
			return ad, nil
		}
	}
}

// askExposure offers the cluster's Gateways next to Ingress and returns the
// chosen Gateway as namespace/name, or "" for Ingress. A Gateway is the
// default when the cluster has no IngressClass and is not GKE, whose built-in
// controller serves Ingresses without one.
func askExposure(in *prompt.Prompter, cluster *detect.Cluster, platform string) (string, error) {
	ingress := "Ingresses"
	if len(cluster.IngressClasses) > 0 {
		ingress += " (classes: " + strings.Join(cluster.IngressClasses, ", ") + ")"
	} else if platform != "gcp" {
		ingress += " (the cluster has no IngressClass)"
	}
	options := []string{ingress}
	for _, g := range cluster.Gateways {
		opt := "HTTPRoutes on Gateway " + g.Ref()
		if _, target := g.DNS(); target != "" {
			opt += " (" + g.Class + ", " + target + ")"
		}
		options = append(options, opt)
	}
	def := 0
	if len(cluster.IngressClasses) == 0 && platform != "gcp" {
		def = 1
	}
	i, err := in.Choose("How should the endpoints be exposed?", options, def)
	if err != nil || i == 0 {
		return "", err
	}
	return cluster.Gateways[i-1].Ref(), nil
}

// exposure describes Gateway API exposure for the install summary, or "" for
// Ingress.
func exposure(vals map[string]any) string {
	refs := gatewayRefs(vals)
	if refs == nil {
		return ""
	}
	return "HTTPRoutes on Gateway " + strings.Join(refs, ", ")
}

// gatewayRefs are the Gateways the endpoints attach to, as
// <namespace>/<name>[:<listener>], or nil when they are Ingresses.
func gatewayRefs(vals map[string]any) []string {
	if p, _ := values.Get(vals, "global", "ingress", "provider").(string); p != "gateway-api" {
		return nil
	}
	refs := []string{}
	parents, _ := values.Get(vals, "global", "ingress", "gatewayApi", "parentRefs").([]any)
	for _, p := range parents {
		m, _ := p.(map[string]any)
		ref := gatewayapi.ParentRef{}
		ref.Name, _ = m["name"].(string)
		ref.Namespace, _ = m["namespace"].(string)
		ref.SectionName, _ = m["sectionName"].(string)
		refs = append(refs, ref.String())
	}
	return refs
}

func askMirror(in *prompt.Prompter, print bool) (setup.Images, error) {
	var img setup.Images
	var err error
	if img.Registry, err = in.Text("Registry address, with the path the images are under (e.g. registry.example.com/neuraltrust)", ""); err != nil {
		return img, err
	}
	auth, err := in.Choose("How does the cluster authenticate to "+setup.RegistryServer(img.Registry)+"?", []string{
		"Username and password (a pull Secret is created)",
		"The nodes' own identity (ECR, Artifact Registry, ACR); no pull Secret",
	}, 0)
	if err != nil {
		return img, err
	}
	if auth == 1 {
		img.NodeIdentity = true
		return img, nil
	}
	if img.Username, err = in.Text("Registry username", ""); err != nil {
		return img, err
	}
	if print {
		return img, nil
	}
	img.Password, err = in.Secret("Registry password or token")
	return img, err
}

// askDatastore returns nil to keep the datastore in the cluster, or the parsed
// external one, from the environment or the operator.
func askDatastore[T any](in *prompt.Prompter, out io.Writer, name, env, question string, parse func(string) (*T, error)) (*T, error) {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		choice, err := in.Choose(name+":", []string{"Run it in the cluster", "Use my own " + name}, 0)
		if err != nil || choice == 0 {
			return nil, err
		}
	}
	for {
		if raw == "" {
			var err error
			if raw, err = in.Secret(question); err != nil {
				return nil, err
			}
		}
		v, err := parse(raw)
		if err == nil {
			return v, nil
		}
		if !in.Interactive || os.Getenv(env) != "" {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		fmt.Fprintf(out, "  %v\n", err)
		raw = ""
	}
}

// exists reports whether a Secret is already in place and should be kept.
func exists(ctx context.Context, cs kubernetes.Interface, ns, name string, replace bool) (bool, error) {
	_, ok, err := secrets.Keys(ctx, cs, ns, name)
	if err != nil {
		return false, err
	}
	return ok && !replace, nil
}

func index(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return len(list) - 1
}

// instanceName is the name offered for a new private instance: the domain,
// else the release name.
func instanceName(vals map[string]any, release string) string {
	if d, _ := values.Get(vals, "global", "domain").(string); d != "" {
		return d
	}
	return release
}

// referenced reports whether the values point a product at the named Secret,
// i.e. whether ntctl is the one expected to create it.
func referenced(vals map[string]any, name string) bool {
	for _, root := range []string{"agentgateway", "trustguard"} {
		if values.Get(vals, root, "configSync", "existingSecret", "name") == name ||
			values.Get(vals, root, "dataagent", "enrolment", "existingSecret", "name") == name {
			return true
		}
	}
	return false
}

func (f installFlags) print() bool { return f.secretsMode == "print" }

// printSecrets is the end of an install with --secrets-mode print: it saves
// the values and prints the commands that create the Secrets still missing,
// changing nothing in the cluster.
func printSecrets(ctx context.Context, out io.Writer, cs kubernetes.Interface, ns string, create []*corev1.Secret, keyFile, file string, vals map[string]any) error {
	var missing []*corev1.Secret
	for _, s := range create {
		keep, err := exists(ctx, cs, ns, s.Name, false)
		if err != nil {
			return err
		}
		if keep {
			fmt.Fprintf(out, "Secret %s is already in place.\n", s.Name)
			continue
		}
		missing = append(missing, s)
	}
	if err := values.Save(file, vals); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nSaved the values to %s (no credentials). Nothing was changed in the cluster.\n", file)
	if len(missing) > 0 {
		fmt.Fprintln(out, "\nCreate these Secrets, replacing every <…> placeholder with the real value:")
		fmt.Fprintln(out)
		if ok, err := secrets.HasNamespace(ctx, cs, ns); err != nil {
			return err
		} else if !ok {
			fmt.Fprintf(out, "kubectl create namespace %s\n", ns)
		}
		fmt.Fprint(out, secrets.Commands(missing, keyFile))
	}
	fmt.Fprintf(out, "\nThen install with:\n  ntctl install -n %s -f %s\n", ns, file)
	return nil
}
