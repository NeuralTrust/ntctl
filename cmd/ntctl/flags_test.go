package main

import (
	"os"
	"path/filepath"
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
