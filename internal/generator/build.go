package generator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/manifest"
)

// Lookup returns a live Secret of the install's namespace, or nil when it does
// not exist. A nil Lookup stands for an empty namespace.
type Lookup func(ctx context.Context, name string) (*corev1.Secret, error)

// Owner says who holds a Secret the generator writes, as the generator decides
// it: absent, its own (managed), the operator's (external), or rendered by an
// earlier chart, which the generator would take over.
type Owner string

// Owners.
const (
	Absent   Owner = "absent"
	Managed  Owner = "managed"
	External Owner = "external"
	Rendered Owner = "rendered"
)

// Secret is one Secret the generator writes, with the values it would hold
// after the generator's next run.
type Secret struct {
	Name  string
	Type  corev1.SecretType
	Owner Owner
	Data  map[string]string
	// Live are the keys it holds now with these same values; Origin says,
	// for every other key, where its value comes from: "generated",
	// "from values", or "adopted from <secret>".
	Live   []string
	Origin map[string]string
	// Missing are the keys an external Secret lacks, which the generator
	// refuses to write for the operator.
	Missing []string
	// Annotations are the ones the generator sets beyond its own metadata,
	// such as a certificate's SANs digest.
	Annotations map[string]string
}

// Plan is everything the generator of a rendered release would write.
type Plan struct {
	Secrets []Secret
}

// job is what Build reads from the generator Job's environment.
type job struct {
	target, previous string
	previousTLS      map[string]string
	components       []string
	operator         []string
	specLabel        string
	specFormat       string
}

func readJob(objs []manifest.Object) (job, bool) {
	for _, o := range objs {
		if o.Kind() != "Job" {
			continue
		}
		env := map[string]string{}
		list, _ := o.PodSpec()["containers"].([]any)
		for _, c := range list {
			cm, _ := c.(map[string]any)
			vars, _ := cm["env"].([]any)
			for _, e := range vars {
				m, _ := e.(map[string]any)
				n, _ := m["name"].(string)
				v, _ := m["value"].(string)
				env[n] = v
			}
		}
		if env[targetEnv] == "" {
			continue
		}
		j := job{target: env[targetEnv], previous: env["PREVIOUS_SECRET"], previousTLS: map[string]string{},
			components: strings.Fields(env["SPEC_COMPONENTS"]), operator: strings.Fields(env["OPERATOR_SECRETS"]),
			specLabel: env["SPEC_LABEL"], specFormat: env["SPEC_FORMAT"]}
		if raw := env["PREVIOUS_TLS"]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &j.previousTLS) // optional: without it no earlier certificate is adopted
		}
		return j, true
	}
	return job{}, false
}

// ErrNoGenerator means the rendered release runs no secrets generator: the
// chart renders its Secrets itself for these values.
var ErrNoGenerator = errors.New("the chart runs no secrets generator for these values")

// Build works out what the generator in objs, a release rendered with its
// hooks, would write into the namespace live reads from. It follows the
// generator's own rules, so the result is what its next run would leave:
// values already there stand, then the earlier generator's Secret, values
// pinned in the chart values, the Secrets earlier charts rendered them into,
// and a new value only for what is still missing. Nothing is written.
func Build(ctx context.Context, objs []manifest.Object, live Lookup) (*Plan, error) {
	j, ok := readJob(objs)
	if !ok {
		return nil, ErrNoGenerator
	}
	if live == nil {
		live = func(context.Context, string) (*corev1.Secret, error) { return nil, nil }
	}
	entries, pins, err := spec(objs, j)
	if err != nil {
		return nil, err
	}
	sources := map[string]*corev1.Secret{}
	source := func(name string) (map[string]string, bool, error) {
		if name == "" {
			return nil, false, nil
		}
		s, seen := sources[name]
		if !seen {
			var err error
			if s, err = live(ctx, name); err != nil {
				return nil, false, fmt.Errorf("reading Secret %s: %w", name, err)
			}
			sources[name] = s
		}
		if s == nil {
			return nil, false, nil
		}
		return decode(s), true, nil
	}
	owner := func(name string) (Owner, error) {
		if _, ok, err := source(name); err != nil || !ok {
			return Absent, err
		}
		s := sources[name]
		switch {
		case s.Annotations[Marker] == "managed":
			return Managed, nil
		case slices.Contains(j.operator, name) || s.Annotations[Marker] == "external" || len(s.OwnerReferences) > 0:
			return External, nil
		}
		return Rendered, nil
	}

	var scalars, certs []Entry
	for _, e := range entries {
		if e.Type == "tls" {
			certs = append(certs, e)
		} else {
			scalars = append(scalars, e)
		}
	}
	plan := &Plan{}
	if len(scalars) > 0 {
		s, err := buildStore(j, scalars, pins, owner, source)
		if err != nil {
			return nil, err
		}
		plan.Secrets = append(plan.Secrets, s)
	}
	for _, e := range certs {
		s, err := buildTLS(j, e, owner, source)
		if err != nil {
			return nil, err
		}
		plan.Secrets = append(plan.Secrets, s)
	}
	return plan, nil
}

// spec returns the entries and pinned values of the components the Job
// works on, from the spec objects this chart rendered.
func spec(objs []manifest.Object, j job) ([]Entry, map[string]string, error) {
	cms := map[string]manifest.Object{}
	pinned := map[string]manifest.Object{}
	// Spec objects without the format label are an earlier chart's, which
	// the generator ignores; charts before the label have none to check.
	current := func(o manifest.Object) bool {
		meta, _ := o["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		return j.specLabel == "" || fmt.Sprint(labels[j.specLabel]) == j.specFormat
	}
	for _, o := range objs {
		name := o.Name()
		switch {
		case o.Kind() == "ConfigMap" && strings.HasPrefix(name, specPrefix) && current(o):
			cms[strings.TrimPrefix(name, specPrefix)] = o
		case o.Kind() == "Secret" && strings.HasPrefix(name, pinsPrefix) && current(o):
			pinned[strings.TrimPrefix(name, pinsPrefix)] = o
		}
	}
	components := j.components
	if len(components) == 0 {
		for c := range cms {
			components = append(components, c)
		}
		sort.Strings(components)
	}
	var entries []Entry
	pins := map[string]string{}
	for _, c := range components {
		cm, ok := cms[c]
		if !ok {
			continue
		}
		got, err := Spec([]manifest.Object{cm})
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, got...)
		if p, ok := pinned[c]; ok {
			for k, v := range objectData(p) {
				pins[k] = v
			}
		}
	}
	return entries, pins, nil
}

const pinsPrefix = "neuraltrust-generator-pins-"

func buildStore(j job, scalars []Entry, pins map[string]string, owner func(string) (Owner, error), source func(string) (map[string]string, bool, error)) (Secret, error) {
	who, err := owner(j.target)
	if err != nil {
		return Secret{}, err
	}
	current, _, err := source(j.target)
	if err != nil {
		return Secret{}, err
	}
	out := Secret{Name: j.target, Type: corev1.SecretTypeOpaque, Owner: who, Origin: map[string]string{}}
	if who == External {
		out.Data = current
		for _, e := range scalars {
			if e.Type == "adopt" || (e.Type == "alias" && current[e.Source] == "") || current[e.Key] != "" {
				continue
			}
			out.Missing = append(out.Missing, e.Key)
		}
		out.Live = sortedKeys(current)
		return out, nil
	}
	next := copyMap(current)
	previous, hadPrevious, err := source(j.previous)
	if err != nil {
		return Secret{}, err
	}
	// A Secret an earlier chart rendered holds what that chart rendered; the
	// earlier generator's copy holds what the services read since, so it
	// wins.
	if who != Managed {
		for k, v := range previous {
			next[k] = v
			if current[k] != v {
				out.Origin[k] = "adopted from " + j.previous
			}
		}
	}
	late := func(e Entry) bool { return e.Type == "alias" || e.Type == "derived" }
	ordered := append(slices.DeleteFunc(slices.Clone(scalars), late), slices.DeleteFunc(slices.Clone(scalars), func(e Entry) bool { return !late(e) })...)
	for _, e := range ordered {
		k := e.Key
		switch {
		case late(e):
			if src := next[e.Source]; src != "" {
				v := src
				if e.Type == "derived" {
					v = strings.Replace(e.Format, "%s", src, 1)
				}
				if next[k] != v {
					out.Origin[k] = "from " + e.Source
				}
				next[k] = v
			}
			continue
		case pins[k] != "":
			if next[k] != pins[k] {
				out.Origin[k] = "from values"
			}
			next[k] = pins[k]
			continue
		case next[k] != "":
			continue
		}
		adopted := false
		for _, pair := range e.Adopt {
			if len(pair) != 2 {
				continue
			}
			d, _, err := source(pair[0])
			if err != nil {
				return Secret{}, err
			}
			if v := d[pair[1]]; v != "" {
				next[k], out.Origin[k], adopted = v, "adopted from "+pair[0], true
				break
			}
		}
		if adopted || e.Type == "adopt" {
			continue
		}
		if e.Type == "install" {
			predates := hadPrevious
			for _, name := range e.FreshIf {
				if _, ok, err := source(name); err != nil {
					return Secret{}, err
				} else if ok {
					predates = true
				}
			}
			if predates {
				continue
			}
		}
		v, err := generate(e)
		if err != nil {
			return Secret{}, err
		}
		next[k], out.Origin[k] = v, "generated"
	}
	out.Data = next
	for k, v := range current {
		if next[k] == v {
			out.Live = append(out.Live, k)
		}
	}
	sort.Strings(out.Live)
	return out, nil
}

// complete reports whether a TLS Secret's data is usable as it is.
func complete(e Entry, d map[string]string) bool {
	return d["tls.crt"] != "" && d["tls.key"] != "" && (e.SelfSigned || d["ca.crt"] != "")
}

func buildTLS(j job, e Entry, owner func(string) (Owner, error), source func(string) (map[string]string, bool, error)) (Secret, error) {
	who, err := owner(e.Secret)
	if err != nil {
		return Secret{}, err
	}
	own, _, err := source(e.Secret)
	if err != nil {
		return Secret{}, err
	}
	out := Secret{Name: e.Secret, Type: corev1.SecretTypeTLS, Owner: who, Origin: map[string]string{},
		Annotations: map[string]string{"neuraltrust.ai/tls-sans": sansHash(e.SANs)}}
	keep := func(d map[string]string) Secret {
		out.Data, out.Live = d, sortedKeys(d)
		return out
	}
	switch {
	case who == External && !complete(e, own):
		out.Data, out.Live = own, sortedKeys(own)
		for _, k := range []string{"tls.crt", "tls.key", "ca.crt"} {
			if own[k] == "" && (k != "ca.crt" || !e.SelfSigned) {
				out.Missing = append(out.Missing, k)
			}
		}
		return out, nil
	case who == External, who == Managed && complete(e, own):
		return keep(own), nil
	}
	candidates := []string{}
	if p := j.previousTLS[e.Secret]; p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, e.Secret)
	for _, pair := range e.Adopt {
		if len(pair) > 0 && pair[0] != e.Secret {
			candidates = append(candidates, pair[0])
		}
	}
	for _, name := range candidates {
		d, _, err := source(name)
		if err != nil {
			return Secret{}, err
		}
		if !complete(e, d) {
			continue
		}
		data := map[string]string{"tls.crt": d["tls.crt"], "tls.key": d["tls.key"], "ca.crt": d["ca.crt"]}
		if data["ca.crt"] == "" {
			data["ca.crt"] = d["tls.crt"]
		}
		out.Data = data
		for k, v := range data {
			if own[k] == v {
				out.Live = append(out.Live, k)
			} else {
				out.Origin[k] = "adopted from " + name
			}
		}
		sort.Strings(out.Live)
		return out, nil
	}
	data, err := issue(e)
	if err != nil {
		return Secret{}, err
	}
	out.Data = data
	for k := range data {
		out.Origin[k] = "generated"
	}
	return out, nil
}

// alnumChars is the generator's alphabet; 248 is the largest multiple of 62
// below 256, so rejecting bytes from 248 up keeps every character equally
// likely.
const alnumChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func alnum(n int) (string, error) {
	var b strings.Builder
	buf := make([]byte, n*2)
	for b.Len() < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		for _, c := range buf {
			if c < 248 {
				b.WriteByte(alnumChars[int(c)%62])
				if b.Len() == n {
					break
				}
			}
		}
	}
	return b.String(), nil
}

// generate makes a new value of the entry's type, as the generator does.
func generate(e Entry) (string, error) {
	switch e.Type {
	case "alnum", "install":
		n := e.Length
		if n == 0 {
			n = 64
		}
		return alnum(n)
	case "base64":
		n := e.Length
		if n == 0 {
			n = 32
		}
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		return base64.StdEncoding.EncodeToString(b), nil
	case "rsa-pkcs1", "rsa-pkcs8":
		k, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return "", fmt.Errorf("generating %s: %w", e.Key, err)
		}
		if e.Type == "rsa-pkcs1" {
			return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})), nil
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return "", fmt.Errorf("encoding %s: %w", e.Key, err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
	}
	return "", fmt.Errorf("unknown type %s for %s: this ntctl predates the chart's generator; update ntctl", e.Type, e.Key)
}

// sansHash is the digest the chart's TLS templates annotate: sha256 of the
// comma-joined SANs, 16 hex characters.
func sansHash(sans []string) string {
	h := sha256.Sum256([]byte(strings.Join(sans, ",")))
	return hex.EncodeToString(h[:])[:16]
}

// issue makes the certificate the generator would: RSA 2048, valid for
// days (default 3650), either self-signed and its own CA, as openssl req
// -x509 makes one, or signed by a new CA that ca.crt carries.
func issue(e Entry) (map[string]string, error) {
	days := e.Days
	if days == 0 {
		days = 3650
	}
	now := time.Now()
	leaf := &x509.Certificate{
		Subject: pkix.Name{CommonName: e.CN}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, days),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, s := range e.SANs {
		if ip := net.ParseIP(s); ip != nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		} else {
			leaf.DNSNames = append(leaf.DNSNames, s)
		}
	}
	key, err := newKey(e.Secret)
	if err != nil {
		return nil, err
	}
	if leaf.SerialNumber, err = serial(); err != nil {
		return nil, err
	}
	parent, signer, caPEM := leaf, key, ""
	if e.SelfSigned {
		leaf.IsCA = true
		leaf.KeyUsage |= x509.KeyUsageCertSign
	} else {
		if signer, err = newKey(e.Secret + " CA"); err != nil {
			return nil, err
		}
		tmpl := &x509.Certificate{
			Subject: pkix.Name{CommonName: e.CACN}, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		if tmpl.SerialNumber, err = serial(); err != nil {
			return nil, err
		}
		caDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &signer.PublicKey, signer)
		if err != nil {
			return nil, fmt.Errorf("issuing the CA of %s: %w", e.Secret, err)
		}
		if parent, err = x509.ParseCertificate(caDER); err != nil {
			return nil, fmt.Errorf("reading the CA of %s: %w", e.Secret, err)
		}
		caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, fmt.Errorf("issuing %s: %w", e.Secret, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encoding the key of %s: %w", e.Secret, err)
	}
	crt := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if caPEM == "" {
		caPEM = crt
	}
	return map[string]string{"tls.crt": crt, "tls.key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), "ca.crt": caPEM}, nil
}

func newKey(what string) (*rsa.PrivateKey, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating the key of %s: %w", what, err)
	}
	return k, nil
}

func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("making a serial number: %w", err)
	}
	return n, nil
}

func decode(s *corev1.Secret) map[string]string {
	out := map[string]string{}
	for k, v := range s.Data {
		out[k] = string(v)
	}
	for k, v := range s.StringData {
		out[k] = v
	}
	return out
}

// objectData decodes a rendered Secret's data and stringData.
func objectData(o manifest.Object) map[string]string {
	out := map[string]string{}
	if d, ok := o["data"].(map[string]any); ok {
		for k, v := range d {
			s, _ := v.(string)
			if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
				out[k] = string(raw)
			}
		}
	}
	if d, ok := o["stringData"].(map[string]any); ok {
		for k, v := range d {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
