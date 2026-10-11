package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/NeuralTrust/ntctl/internal/chart"
	"github.com/NeuralTrust/ntctl/internal/kube"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// errHelp is returned after -h printed the flags.
var errHelp = errors.New("help requested")

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// opts are the flags most commands share.
type opts struct {
	kube       kube.Options
	release    string
	chartPath  string
	version    string
	valueFiles stringList
	domain     string
	platform   string
	products   string
	output     string
	// extra are values from interactive answers, layered like explicit flags.
	extra map[string]any
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
		fs.StringVar(&o.domain, "domain", "", "base domain for the endpoints, e.g. platform.example.com")
		fs.StringVar(&o.platform, "platform", "", "aws, gcp, azure, openshift or kubernetes (default: detected from the cluster)")
		fs.StringVar(&o.products, "products", "", "comma-separated products: "+strings.Join(values.Products, ","))
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
	if o.products == "" {
		return nil, nil
	}
	var out []string
	for _, p := range strings.Split(o.products, ",") {
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
	vals := values.Generate(defaults)
	vals = values.Merge(vals, files)
	vals = values.Merge(vals, values.Generate(values.Inputs{Domain: o.domain, Platform: o.platform, Products: products}))
	if o.extra != nil {
		vals = values.Merge(vals, copyMap(o.extra))
	}
	return vals, nil
}
