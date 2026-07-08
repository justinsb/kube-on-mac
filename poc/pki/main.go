// pki is the declarative PKI reconciler for kube-on-macos: the cluster's
// entire identity is a directory of YAML spec files, each describing one
// certificate, keypair, or kubeconfig, with the generated material living
// alongside its spec (apiserver.yaml -> apiserver.crt + apiserver.key).
//
//	pki --dir etc/kubernetes
//
// It reconciles, kubectl-apply style: missing material is created, material
// matching its spec is left untouched (byte-stable across runs), material
// that drifted — SANs changed, CA rotated, expiring soon, key mismatch — is
// regenerated, and anything depending on a regenerated CA follows
// automatically (child certs fail issuer verification, kubeconfigs embed new
// bytes). There are no phases and no hidden defaulting: what the files say
// is what exists. See research/static-pod-control-plane.md.
//
// Field names follow cert-manager's Certificate where they overlap
// (commonName, dnsNames, ipAddresses, isCA, issuerRef, usages, duration) —
// familiar and declarative — under our own apiVersion, because this is a
// file reconciler, not a controller.
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const apiVersion = "kube-on-macos.io/v1alpha1"

type spec struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		// Certificate
		IsCA          bool     `json:"isCA,omitempty"`
		IssuerRef     string   `json:"issuerRef,omitempty"` // spec name (path sans .yaml, relative to --dir)
		CommonName    string   `json:"commonName,omitempty"`
		Organizations []string `json:"organizations,omitempty"`
		DNSNames      []string `json:"dnsNames,omitempty"`
		IPAddresses   []string `json:"ipAddresses,omitempty"`
		Usages        []string `json:"usages,omitempty"`   // "server auth", "client auth"
		Duration      string   `json:"duration,omitempty"` // e.g. "87600h"; default 10y

		// KeyPair
		Algorithm string `json:"algorithm,omitempty"` // "rsa2048" (default) or "ecdsa-p256"

		// Kubeconfig
		Server                  string `json:"server,omitempty"`
		CertificateAuthorityRef string `json:"certificateAuthorityRef,omitempty"`
		ClientCertificateRef    string `json:"clientCertificateRef,omitempty"`
	} `json:"spec"`

	name string // path sans .yaml, relative to --dir
	dir  string // absolute dir containing the spec file
}

func main() {
	dir := flag.String("dir", ".", "root directory of pki specs (walked recursively)")
	flag.Parse()

	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	specs, err := loadSpecs(root)
	if err != nil {
		log.Fatal(err)
	}
	if len(specs) == 0 {
		log.Fatalf("no pki specs (apiVersion %s) found under %s", apiVersion, root)
	}

	r := &reconciler{root: root, specs: specs}
	// Order matters only by dependency: keypairs and CAs first, then leaf
	// certificates, then kubeconfigs (which embed certificate bytes).
	for _, pass := range []func(*spec) bool{
		func(s *spec) bool { return s.Kind == "KeyPair" },
		func(s *spec) bool { return s.Kind == "Certificate" && s.Spec.IsCA },
		func(s *spec) bool { return s.Kind == "Certificate" && !s.Spec.IsCA },
		func(s *spec) bool { return s.Kind == "Kubeconfig" },
	} {
		for _, s := range specs {
			if !pass(s) {
				continue
			}
			if err := r.reconcile(s); err != nil {
				log.Fatalf("%s: %v", s.name, err)
			}
		}
	}
}

func loadSpecs(root string) ([]*spec, error) {
	var specs []*spec
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s := &spec{}
		if err := yaml.Unmarshal(data, s); err != nil || s.APIVersion != apiVersion {
			return nil // not ours (e.g. a pod manifest); skip
		}
		switch s.Kind {
		case "Certificate", "KeyPair", "Kubeconfig":
		default:
			return fmt.Errorf("%s: unknown kind %q", path, s.Kind)
		}
		rel, _ := filepath.Rel(root, path)
		s.name = strings.TrimSuffix(rel, ".yaml")
		s.dir = filepath.Dir(path)
		specs = append(specs, s)
		return nil
	})
	sort.Slice(specs, func(i, j int) bool { return specs[i].name < specs[j].name })
	return specs, err
}

type reconciler struct {
	root  string
	specs []*spec
}

func (r *reconciler) find(name string) *spec {
	for _, s := range r.specs {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (r *reconciler) path(name, ext string) string {
	return filepath.Join(r.root, name+ext)
}

func (r *reconciler) reconcile(s *spec) error {
	switch s.Kind {
	case "KeyPair":
		return r.reconcileKeyPair(s)
	case "Certificate":
		return r.reconcileCertificate(s)
	case "Kubeconfig":
		return r.reconcileKubeconfig(s)
	}
	return nil
}

// --- KeyPair ---

func (r *reconciler) reconcileKeyPair(s *spec) error {
	keyPath, pubPath := r.path(s.name, ".key"), r.path(s.name, ".pub")
	if key, err := loadKey(keyPath); err == nil {
		if pub, err := os.ReadFile(pubPath); err == nil && string(pub) == string(marshalPublic(key)) {
			log.Printf("%-28s ok", s.name)
			return nil
		}
		// .pub missing/stale: rewrite from the existing key.
		if err := os.WriteFile(pubPath, marshalPublic(key), 0o644); err != nil {
			return err
		}
		log.Printf("%-28s public key rewritten", s.name)
		return nil
	}
	key, err := generateKey(s.Spec.Algorithm)
	if err != nil {
		return err
	}
	if err := writeKey(keyPath, key); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, marshalPublic(key), 0o644); err != nil {
		return err
	}
	log.Printf("%-28s created (%s)", s.name, algName(s.Spec.Algorithm))
	return nil
}

// --- Certificate ---

func (r *reconciler) reconcileCertificate(s *spec) error {
	crtPath, keyPath := r.path(s.name, ".crt"), r.path(s.name, ".key")

	var issuerCert *x509.Certificate
	var issuerKey crypto.Signer
	if !s.Spec.IsCA || s.Spec.IssuerRef != "" {
		ref := s.Spec.IssuerRef
		if ref == "" {
			return fmt.Errorf("non-CA certificate needs issuerRef")
		}
		issuer := r.find(ref)
		if issuer == nil || issuer.Kind != "Certificate" || !issuer.Spec.IsCA {
			return fmt.Errorf("issuerRef %q is not a CA Certificate spec", ref)
		}
		var err error
		issuerCert, err = loadCert(r.path(ref, ".crt"))
		if err != nil {
			return fmt.Errorf("issuer %s not materialized: %w", ref, err)
		}
		issuerKey, err = loadKey(r.path(ref, ".key"))
		if err != nil {
			return fmt.Errorf("issuer %s key: %w", ref, err)
		}
	}

	if reason := r.certDrift(s, crtPath, keyPath, issuerCert); reason == "" {
		log.Printf("%-28s ok", s.name)
		return nil
	} else if _, err := os.Stat(crtPath); err == nil {
		log.Printf("%-28s regenerating: %s", s.name, reason)
	}

	// Keep an existing key when only the certificate drifted.
	key, err := loadKey(keyPath)
	if err != nil {
		if key, err = generateKey("ecdsa-p256"); err != nil {
			return err
		}
		if err := writeKey(keyPath, key); err != nil {
			return err
		}
	}

	tmpl, err := certTemplate(s)
	if err != nil {
		return err
	}
	parent, signer := tmpl, key
	if issuerCert != nil {
		parent, signer = issuerCert, issuerKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, key.Public(), signer)
	if err != nil {
		return err
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(crtPath, crt, 0o644); err != nil {
		return err
	}
	log.Printf("%-28s issued (CN=%s%s)", s.name, s.Spec.CommonName, issuedBy(s))
	return nil
}

func issuedBy(s *spec) string {
	if s.Spec.IssuerRef != "" {
		return ", by " + s.Spec.IssuerRef
	}
	return ", self-signed CA"
}

// certDrift returns "" when the materialized cert+key match the spec, else
// the reason regeneration is needed. This is the declarative heart: the
// YAML is the desired state, the PEM files are the observed state.
func (r *reconciler) certDrift(s *spec, crtPath, keyPath string, issuer *x509.Certificate) string {
	cert, err := loadCert(crtPath)
	if err != nil {
		return "missing"
	}
	key, err := loadKey(keyPath)
	if err != nil {
		return "key missing"
	}
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return "certificate does not match key"
	}
	if cert.Subject.CommonName != s.Spec.CommonName {
		return fmt.Sprintf("commonName %q != %q", cert.Subject.CommonName, s.Spec.CommonName)
	}
	if !stringSetEqual(cert.Subject.Organization, s.Spec.Organizations) {
		return fmt.Sprintf("organizations %v != %v", cert.Subject.Organization, s.Spec.Organizations)
	}
	if cert.IsCA != s.Spec.IsCA {
		return "isCA changed"
	}
	if !stringSetEqual(cert.DNSNames, s.Spec.DNSNames) {
		return fmt.Sprintf("dnsNames %v != %v", cert.DNSNames, s.Spec.DNSNames)
	}
	var wantIPs []string
	for _, ip := range s.Spec.IPAddresses {
		if p := net.ParseIP(ip); p != nil {
			wantIPs = append(wantIPs, p.String())
		}
	}
	var haveIPs []string
	for _, ip := range cert.IPAddresses {
		haveIPs = append(haveIPs, ip.String())
	}
	if !stringSetEqual(haveIPs, wantIPs) {
		return fmt.Sprintf("ipAddresses %v != %v", haveIPs, wantIPs)
	}
	if !extKeyUsagesEqual(cert.ExtKeyUsage, s) {
		return "usages changed"
	}
	if issuer != nil {
		if err := cert.CheckSignatureFrom(issuer); err != nil {
			return "no longer signed by issuer (CA rotated?)"
		}
	} else if err := cert.CheckSignatureFrom(cert); err != nil {
		return "self-signature invalid"
	}
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		return fmt.Sprintf("expires soon (%s)", cert.NotAfter.Format(time.DateOnly))
	}
	return ""
}

func certTemplate(s *spec) (*x509.Certificate, error) {
	duration := 10 * 365 * 24 * time.Hour
	if s.Spec.Duration != "" {
		d, err := time.ParseDuration(s.Spec.Duration)
		if err != nil {
			return nil, fmt.Errorf("duration: %w", err)
		}
		duration = d
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   s.Spec.CommonName,
			Organization: s.Spec.Organizations,
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(duration),
		BasicConstraintsValid: true,
	}
	for _, name := range s.Spec.DNSNames {
		tmpl.DNSNames = append(tmpl.DNSNames, name)
	}
	for _, ip := range s.Spec.IPAddresses {
		p := net.ParseIP(ip)
		if p == nil {
			return nil, fmt.Errorf("bad ipAddress %q", ip)
		}
		tmpl.IPAddresses = append(tmpl.IPAddresses, p)
	}
	if s.Spec.IsCA {
		tmpl.IsCA = true
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature
		return tmpl, nil
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	tmpl.ExtKeyUsage = wantExtKeyUsages(s)
	return tmpl, nil
}

func wantExtKeyUsages(s *spec) []x509.ExtKeyUsage {
	if s.Spec.IsCA {
		return nil
	}
	if len(s.Spec.Usages) == 0 {
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	var out []x509.ExtKeyUsage
	for _, u := range s.Spec.Usages {
		switch u {
		case "server auth":
			out = append(out, x509.ExtKeyUsageServerAuth)
		case "client auth":
			out = append(out, x509.ExtKeyUsageClientAuth)
		}
	}
	return out
}

func extKeyUsagesEqual(have []x509.ExtKeyUsage, s *spec) bool {
	want := wantExtKeyUsages(s)
	if len(have) != len(want) {
		return false
	}
	set := map[x509.ExtKeyUsage]bool{}
	for _, u := range have {
		set[u] = true
	}
	for _, u := range want {
		if !set[u] {
			return false
		}
	}
	return true
}

// --- Kubeconfig ---

func (r *reconciler) reconcileKubeconfig(s *spec) error {
	if s.Spec.Server == "" || s.Spec.CertificateAuthorityRef == "" || s.Spec.ClientCertificateRef == "" {
		return fmt.Errorf("kubeconfig needs server, certificateAuthorityRef, clientCertificateRef")
	}
	ca, err := os.ReadFile(r.path(s.Spec.CertificateAuthorityRef, ".crt"))
	if err != nil {
		return err
	}
	crt, err := os.ReadFile(r.path(s.Spec.ClientCertificateRef, ".crt"))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(r.path(s.Spec.ClientCertificateRef, ".key"))
	if err != nil {
		return err
	}
	cert, err := loadCert(r.path(s.Spec.ClientCertificateRef, ".crt"))
	if err != nil {
		return err
	}
	user := cert.Subject.CommonName

	b64 := base64.StdEncoding.EncodeToString
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: kube-on-macos
  cluster:
    certificate-authority-data: %s
    server: %s
contexts:
- name: %s
  context:
    cluster: kube-on-macos
    user: %s
current-context: %s
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
`, b64(ca), s.Spec.Server, filepath.Base(s.name), user, filepath.Base(s.name), user, b64(crt), b64(key))

	out := r.path(s.name, ".kubeconfig")
	if old, err := os.ReadFile(out); err == nil && string(old) == content {
		log.Printf("%-28s ok", s.name)
		return nil
	}
	if err := os.WriteFile(out, []byte(content), 0o600); err != nil {
		return err
	}
	log.Printf("%-28s written (user %s -> %s)", s.name, user, s.Spec.Server)
	return nil
}

// --- key/cert file helpers ---

func algName(a string) string {
	if a == "" {
		return "rsa2048"
	}
	return a
}

func generateKey(algorithm string) (crypto.Signer, error) {
	switch algName(algorithm) {
	case "rsa2048":
		return rsa.GenerateKey(rand.Reader, 2048)
	case "ecdsa-p256":
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		return nil, fmt.Errorf("unknown algorithm %q", algorithm)
	}
}

func writeKey(path string, key crypto.Signer) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

func loadKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s: not a signer", path)
	}
	return signer, nil
}

func marshalPublic(key crypto.Signer) []byte {
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		panic(err) // keys we generated always marshal
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func loadCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type eq interface{ Equal(crypto.PublicKey) bool }
	if pk, ok := a.(eq); ok {
		return pk.Equal(b)
	}
	return false
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
