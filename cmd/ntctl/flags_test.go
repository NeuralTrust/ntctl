package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/values"
)

func TestEffectiveValuesPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v.yaml")
	if err := os.WriteFile(file, []byte("global:\n  platform: gcp\n  domain: from-file.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := opts{valueFiles: stringList{file}, domain: "from-flag.example.com"}
	vals, err := o.effectiveValues(values.Inputs{Platform: "aws", Domain: "default.example.com", Products: values.Products})
	if err != nil {
		t.Fatal(err)
	}
	if got := values.Get(vals, "global", "platform"); got != "gcp" {
		t.Errorf("platform = %v: a file must beat the detected default", got)
	}
	if got := values.Get(vals, "global", "domain"); got != "from-flag.example.com" {
		t.Errorf("domain = %v: an explicit flag must beat the file", got)
	}
	if got := values.Get(vals, "global", "products", "trustguard"); got != true {
		t.Errorf("products default missing: %v", values.Get(vals, "global", "products"))
	}
}

func TestProductList(t *testing.T) {
	o := opts{products: "TrustGate, dataPlane"}
	got, err := o.productList()
	if err != nil || len(got) != 2 || got[0] != "trustgate" || got[1] != "dataPlane" {
		t.Fatalf("productList = %v, %v", got, err)
	}
	if _, err := (&opts{products: "agentgateway"}).productList(); err == nil {
		t.Error("accepted an unknown product")
	}
}

func TestManagedSecretsFollowProducts(t *testing.T) {
	vals := values.Generate(values.Inputs{Products: []string{"trustguard", "dataPlane"}})
	got := managedSecrets(vals)
	if _, ok := got[values.TrustGateConfigSync]; ok {
		t.Error("TrustGate Secret planned for a TrustGuard-only install")
	}
	if _, ok := got[values.TrustGuardEnrolment]; !ok || !referenced(vals, values.TrustGuardEnrolment) {
		t.Error("TrustGuard enrolment Secret not planned")
	}
}

func TestOperationalTelemetryFlag(t *testing.T) {
	cases := []struct {
		args []string
		want any // nil: the values leave the choice to the chart or the release
	}{
		{nil, nil},
		{[]string{"--operational-telemetry=true"}, true},
		{[]string{"--operational-telemetry=false"}, false},
		{[]string{"--operational-telemetry"}, true},
	}
	for _, c := range cases {
		var o opts
		if err := parse(newFlags("upgrade", "", io.Discard, &o, true), c.args); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		vals, err := o.effectiveValues(values.Inputs{})
		if err != nil {
			t.Fatal(err)
		}
		if got := values.Get(vals, "global", "observability", "operationalTelemetry"); got != c.want {
			t.Errorf("%v: operationalTelemetry = %v, want %v", c.args, got, c.want)
		}
	}
	var o opts
	if err := parse(newFlags("install", "", io.Discard, &o, true), []string{"--operational-telemetry=maybe"}); err == nil {
		t.Error("accepted --operational-telemetry=maybe")
	}
}

func TestInstallRecordsOperationalTelemetry(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v.yaml")
	if err := os.WriteFile(file, []byte("global:\n  observability:\n    operationalTelemetry: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	cases := []struct {
		name string
		o    opts
		want bool
	}{
		{"unset is off", opts{}, false},
		{"flag on", opts{telemetry: optionalBool{v: &on}}, true},
		{"flag off", opts{telemetry: optionalBool{v: &off}}, false},
		{"file beats the default", opts{valueFiles: stringList{file}}, true},
		{"flag beats the file", opts{valueFiles: stringList{file}, telemetry: optionalBool{v: &off}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vals, err := c.o.effectiveValues(installDefaults("aws"))
			if err != nil {
				t.Fatal(err)
			}
			saved := filepath.Join(t.TempDir(), "neuraltrust-values.yaml")
			if err := values.Save(saved, vals); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(saved)
			if err != nil {
				t.Fatal(err)
			}
			want := "operationalTelemetry: " + strconv.FormatBool(c.want)
			if !strings.Contains(string(b), want) {
				t.Errorf("saved values do not record %q:\n%s", want, b)
			}
		})
	}
}
