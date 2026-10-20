package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/NeuralTrust/ntctl/internal/chart"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/kube"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// errHelp is returned after -h printed the flags.
var errHelp = errors.New("help requested")

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// optionalBool is a true|false flag that also records whether it was given at
// all, so an unset flag can leave a value to the release or to a question.
type optionalBool struct{ v *bool }

func (b *optionalBool) String() string {
	if b == nil || b.v == nil {
		return ""
	}
	return strconv.FormatBool(*b.v)
}

func (b *optionalBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("want true or false, not %q", s)
	}
	b.v = &v
	return nil
}

// IsBoolFlag lets the bare flag mean true, as with any boolean flag.
func (b *optionalBool) IsBoolFlag() bool { return true }

// opts are the flags most commands share.
type opts struct {
	kube       kube.Options
	release    string
	chartPath  string
	version    string
	valueFiles stringList
	// mode is --mode, global.deploymentMode; "" leaves it to the files.
	mode     string
	domain   string
	platform string
	products string
	gateway  string
	output   string
	// telemetry is --operational-telemetry; unset leaves the choice open.
	telemetry optionalBool
	// ignoreCapacity turns a capacity failure into a warning.
	ignoreCapacity bool
	// extra are values from interactive answers, layered like explicit flags.
	extra map[string]any
	// addingOnly marks install's add-products flow, which keeps every
	// setting of the release and asks about nothing but the products.
	addingOnly bool
}

func newFlags(name, summary string, out io.Writer, o *opts, withValues bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintf(out, "%s\n\nUsage:\n  ntctl %s [flags]\n\nFlags:\n", summary, name)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.kube.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.kube.Context, "context", "", "kubeconfig context to use (default: the current context)")
	fs.StringVar(&o.kube.Namespace, "namespace", "neuraltrust", "namespace of the install")
	fs.StringVar(&o.release, "release", "neuraltrust-platform", "Helm release name")
	if withValues {
		fs.StringVar(&o.chartPath, "chart", "", "local chart directory or .tgz (default: the public NeuralTrust chart registry)")
		fs.StringVar(&o.version, "version", "", "chart version or constraint (default: latest "+chart.Supported+")")
		fs.Var(&o.valueFiles, "f", "values file, as with helm -f (repeatable; later files win)")
		fs.StringVar(&o.mode, "mode", "", "where the control plane runs: hybrid (NeuralTrust hosts it; the chart's default), external (this cluster runs everything; nothing leaves it), or saas (this cluster is a control plane serving data planes elsewhere)")
		fs.StringVar(&o.domain, "domain", "", "base domain for the endpoints, e.g. platform.example.com")
		fs.StringVar(&o.platform, "platform", "", "aws, gcp, azure, openshift or kubernetes (default: detected from the cluster)")
		productsUsage := "comma-separated products: " + strings.Join(values.Products, ",")
		if name == "upgrade" {
			productsUsage = "comma-separated products to turn on: " + strings.Join(values.Products, ",") + "; the others keep their state (see --remove-products)"
		}
		fs.StringVar(&o.products, "products", "", productsUsage)
		fs.BoolVar(&o.ignoreCapacity, "ignore-capacity", false, "treat too little cluster capacity as a warning, for test clusters (pods that do not fit stay Pending)")
		fs.StringVar(&o.gateway, "gateway", "", "expose the endpoints as Gateway API HTTPRoutes on this existing Gateway, <namespace>/<name>[:<listener>], instead of Ingresses")
		fs.Var(&o.telemetry, "operational-telemetry", "true or false: TrustGate and TrustGuard export their own traces and metrics (request rate, errors, latency); in hybrid installs to NeuralTrust, to help support diagnose your gateway, otherwise to your in-cluster collector (default: off on install; upgrade keeps the install's setting)")
	}
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return nil
}

// productList validates --products.
func (o *opts) productList() ([]string, error) {
	return parseProducts(o.products)
}

// parseProducts validates a comma-separated product list.
func parseProducts(list string) ([]string, error) {
	if list == "" {
		return nil, nil
	}
	var out []string
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(p)
		valid := false
		for _, known := range values.Products {
			if strings.EqualFold(p, known) {
				out = append(out, known)
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("unknown product %q (valid: %s)", p, strings.Join(values.Products, ", "))
		}
	}
	return out, nil
}

// effectiveValues layers chart-facing values: defaults, then -f files, then
// explicit flags, the same precedence helm gives --set over -f.
func (o *opts) effectiveValues(defaults values.Inputs) (map[string]any, error) {
	files, err := values.Files(o.kube.Settings(), o.valueFiles)
	if err != nil {
		return nil, err
	}
	products, err := o.productList()
	if err != nil {
		return nil, err
	}
	mode, err := o.effectiveMode(files)
	if err != nil {
		return nil, err
	}
	defaults.Mode = mode
	// Defaults only fill what the files leave unset.
	if values.Get(files, "global", "platform") != nil {
		defaults.Platform = ""
	}
	if values.Get(files, "global", "products") != nil {
		defaults.Products = nil
	}
	if values.Get(files, "global", "domain") != nil {
		defaults.Domain = ""
	}
	var gateway *gatewayapi.ParentRef
	if o.gateway != "" {
		ref, err := gatewayapi.ParseRef(o.gateway)
		if err != nil {
			return nil, err
		}
		gateway = &ref
	}
	vals := values.Generate(defaults)
	vals = values.Merge(vals, files)
	vals = values.Merge(vals, values.Generate(values.Inputs{Mode: mode, Domain: o.domain, Platform: o.platform, Products: products, Gateway: gateway, OperationalTelemetry: o.telemetry.v}))
	if o.extra != nil {
		vals = values.Merge(vals, copyMap(o.extra))
	}
	return vals, nil
}

// effectiveMode is the deployment mode the flags and files settle: --mode,
// else the files' global.deploymentMode, else "" (the chart's default,
// hybrid).
func (o *opts) effectiveMode(files map[string]any) (string, error) {
	if o.mode == "" {
		m, _ := values.Get(files, "global", "deploymentMode").(string)
		return m, nil
	}
	for _, m := range values.Modes {
		if strings.EqualFold(o.mode, m) {
			o.mode = m
			return m, nil
		}
	}
	return "", fmt.Errorf("--mode must be one of %s, not %q", strings.Join(values.Modes, ", "), o.mode)
}
