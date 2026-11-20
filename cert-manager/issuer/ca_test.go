package main

// Tests of everything that needs no cluster, with real keys made in memory: nothing here is a fake, and
// no key is ever written to a file. Each request is a real cert-manager.io/v1 CertificateRequest with a
// real CSR, passed through issuer-lib's own constructor, as the controller does it.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/cert-manager/issuer-lib/controllers/signer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var labRules = rules{zone: "lab.example", maxLifetime: 24 * time.Hour}

func newKey(t *testing.T, alg string) crypto.Signer {
	t.Helper()
	var k crypto.Signer
	var err error
	switch alg {
	case "rsa":
		k, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ecdsa":
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ed25519":
		_, k, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("no such key algorithm: %s", alg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// keyPEM encodes a key as cert-manager does by default: PKCS#1 for RSA, SEC 1 for ECDSA, PKCS#8 for
// Ed25519. With pkcs8 true, every key is PKCS#8.
func keyPEM(t *testing.T, k crypto.Signer, pkcs8 bool) []byte {
	t.Helper()
	var typ string
	var der []byte
	var err error
	switch kk := k.(type) {
	case *rsa.PrivateKey:
		if !pkcs8 {
			typ, der = "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(kk)
		}
	case *ecdsa.PrivateKey:
		if !pkcs8 {
			typ = "EC PRIVATE KEY"
			der, err = x509.MarshalECPrivateKey(kk)
		}
	}
	if der == nil && err == nil {
		typ = "PRIVATE KEY"
		der, err = x509.MarshalPKCS8PrivateKey(k)
	}
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

type pair struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

// newCert makes a certificate signed by parent, or by itself if parent is nil.
func newCert(t *testing.T, cn, alg string, isCA bool, parent *pair, edit func(*x509.Certificate)) *pair {
	t.Helper()
	key := newKey(t, alg)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(240 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	if edit != nil {
		edit(tmpl)
	}
	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, key.Public(), signerKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &pair{cert: c, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

type ask struct {
	cn       string
	dns      []string
	ips      []net.IP
	uris     []string
	emails   []string
	duration time.Duration // 0: none asked, which cert-manager reads as 90 days
	isCA     bool
	usages   []cmapi.KeyUsage
	extra    []pkix.Extension
}

// request builds the CertificateRequest cert-manager would make, with a fresh key and a real CSR.
func request(t *testing.T, a ask) (signer.CertificateRequestObject, crypto.PublicKey) {
	t.Helper()
	key := newKey(t, "ecdsa")
	tmpl := &x509.CertificateRequest{
		Subject:         pkix.Name{CommonName: a.cn},
		DNSNames:        a.dns,
		IPAddresses:     a.ips,
		EmailAddresses:  a.emails,
		ExtraExtensions: a.extra,
	}
	for _, u := range a.uris {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	cr := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "www-1"},
		Spec: cmapi.CertificateRequestSpec{
			Request: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			IsCA:    a.isCA,
			Usages:  a.usages,
		},
	}
	if a.duration != 0 {
		cr.Spec.Duration = &metav1.Duration{Duration: a.duration}
	}
	return signer.CertificateRequestObjectFromCertificateRequest(cr), key.Public()
}

func mustLoad(t *testing.T, crt, key, caCrt []byte) *authority {
	t.Helper()
	a, err := loadAuthority(crt, key, caCrt)
	if err != nil {
		t.Fatalf("loadAuthority: %v", err)
	}
	return a
}

func parseAll(t *testing.T, pemBytes []byte) []*x509.Certificate {
	t.Helper()
	certs, err := certificates(pemBytes)
	if err != nil {
		t.Fatalf("not a certificate bundle: %v", err)
	}
	return certs
}

func TestSignGoodRequest(t *testing.T) {
	for _, alg := range []string{"rsa", "ecdsa", "ed25519"} {
		for _, pkcs8 := range []bool{false, true} {
			name := alg
			if pkcs8 {
				name += "-pkcs8"
			}
			t.Run(name, func(t *testing.T) {
				ca := newCert(t, "Lab Issuer CA", alg, true, nil, nil)
				a := mustLoad(t, ca.pem, keyPEM(t, ca.key, pkcs8), nil)
				s := labSigner{rules: labRules}
				cr, pub := request(t, ask{
					cn:       "www.lab.example",
					dns:      []string{"www.lab.example", "api.demo.lab.example", "lab.example", "*.apps.lab.example"},
					duration: time.Hour,
					usages:   []cmapi.KeyUsage{cmapi.UsageDigitalSignature, cmapi.UsageServerAuth, cmapi.UsageClientAuth},
				})
				bundle, leaf, err := s.issue(a, cr)
				if err != nil {
					t.Fatalf("issue: %v", err)
				}

				chain := parseAll(t, bundle.ChainPEM)
				if len(chain) != 2 || !chain[0].Equal(leaf) || !chain[1].Equal(ca.cert) {
					t.Fatalf("ChainPEM: want the leaf and then the CA, got %d certificates", len(chain))
				}
				if !bytes.Equal(bundle.CAPEM, ca.pem) {
					t.Errorf("CAPEM: want the CA certificate when the Secret has no ca.crt")
				}

				roots := x509.NewCertPool()
				roots.AddCert(ca.cert)
				for _, n := range []string{"www.lab.example", "lab.example", "x.apps.lab.example"} {
					if _, err := leaf.Verify(x509.VerifyOptions{
						Roots: roots, DNSName: n, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
					}); err != nil {
						t.Errorf("the leaf does not verify for %s: %v", n, err)
					}
				}
				if want := []string{"www.lab.example", "api.demo.lab.example", "lab.example", "*.apps.lab.example"}; !slices.Equal(leaf.DNSNames, want) {
					t.Errorf("DNS names: got %v, want %v", leaf.DNSNames, want)
				}
				if leaf.Subject.CommonName != "www.lab.example" {
					t.Errorf("common name: got %q", leaf.Subject.CommonName)
				}
				if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
					t.Errorf("key usage: got %v, want digital signature only", leaf.KeyUsage)
				}
				if want := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}; !slices.Equal(leaf.ExtKeyUsage, want) {
					t.Errorf("extended key usage: got %v, want %v", leaf.ExtKeyUsage, want)
				}
				if leaf.IsCA || !leaf.BasicConstraintsValid {
					t.Errorf("the leaf must say CA:FALSE (IsCA=%v, valid=%v)", leaf.IsCA, leaf.BasicConstraintsValid)
				}
				if got := leaf.NotAfter.Sub(leaf.NotBefore); got != time.Hour {
					t.Errorf("lifetime: got %s, want the 1h asked for", got)
				}
				if !leaf.PublicKey.(*ecdsa.PublicKey).Equal(pub) {
					t.Errorf("the leaf does not carry the CSR's public key")
				}
				if leaf.SerialNumber.Sign() <= 0 {
					t.Errorf("serial: got %s", leaf.SerialNumber)
				}
			})
		}
	}
}

// An intermediate as the CA: tls.crt holds the intermediate, ca.crt the root, as cert-manager writes
// the Secret of an isCA Certificate.
func TestSignFromIntermediate(t *testing.T) {
	root := newCert(t, "Lab Root", "ecdsa", true, nil, nil)
	inter := newCert(t, "Lab Issuer CA", "ecdsa", true, root, nil)
	a := mustLoad(t, inter.pem, keyPEM(t, inter.key, false), root.pem)
	cr, _ := request(t, ask{dns: []string{"www.lab.example"}})
	bundle, leaf, err := labSigner{rules: labRules}.issue(a, cr)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	chain := parseAll(t, bundle.ChainPEM)
	if len(chain) != 2 || !chain[1].Equal(inter.cert) {
		t.Fatalf("ChainPEM: want leaf and intermediate, got %d certificates", len(chain))
	}
	if !bytes.Equal(bundle.CAPEM, root.pem) {
		t.Errorf("CAPEM: want the Secret's ca.crt, the root")
	}
	roots, inters := x509.NewCertPool(), x509.NewCertPool()
	roots.AppendCertsFromPEM(bundle.CAPEM)
	inters.AddCert(chain[1])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inters, DNSName: "www.lab.example"}); err != nil {
		t.Errorf("the leaf does not verify through the intermediate to the root: %v", err)
	}
}

func TestHouseRules(t *testing.T) {
	ca := newCert(t, "Lab Issuer CA", "ecdsa", true, nil, nil)
	a := mustLoad(t, ca.pem, keyPEM(t, ca.key, false), nil)
	s := labSigner{rules: labRules}
	for _, tc := range []struct {
		name string
		ask  ask
		want string // the whole error text, as the reader sees it on the CertificateRequest
	}{
		{"a name outside the zone", ask{dns: []string{"www.lab.example", "api.example.com"}},
			`house rule: "api.example.com" is outside lab.example`},
		{"a name that only ends like the zone", ask{dns: []string{"evillab.example"}},
			`house rule: "evillab.example" is outside lab.example`},
		{"the zone as a prefix", ask{dns: []string{"lab.example.com"}},
			`house rule: "lab.example.com" is outside lab.example`},
		{"a wildcard above the zone", ask{dns: []string{"*.example"}},
			`house rule: "*.example" is outside lab.example`},
		{"no DNS name", ask{cn: "www.lab.example"},
			`house rule: the request has no DNS name: it needs at least one in lab.example`},
		{"an IP address", ask{dns: []string{"www.lab.example"}, ips: []net.IP{net.ParseIP("10.96.0.10")}},
			`house rule: the IP address 10.96.0.10 is refused: DNS names in lab.example only`},
		{"a URI", ask{dns: []string{"www.lab.example"}, uris: []string{"spiffe://lab.example/ns/demo/sa/web"}},
			`house rule: the URI "spiffe://lab.example/ns/demo/sa/web" is refused: DNS names in lab.example only`},
		{"an email address", ask{dns: []string{"www.lab.example"}, emails: []string{"ops@lab.example"}},
			`house rule: the email address "ops@lab.example" is refused: DNS names in lab.example only`},
		{"isCA", ask{dns: []string{"ca.lab.example"}, isCA: true},
			`house rule: isCA is refused: this issuer signs leaf certificates only`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, _ := request(t, tc.ask)
			bundle, leaf, err := s.issue(a, cr)
			if err == nil {
				t.Fatalf("signed, want a refusal")
			}
			if !errors.As(err, &signer.PermanentError{}) {
				t.Errorf("want a signer.PermanentError, got %T", err)
			}
			if err.Error() != tc.want {
				t.Errorf("message:\n got %s\nwant %s", err, tc.want)
			}
			if leaf != nil || len(bundle.ChainPEM) != 0 {
				t.Errorf("a refusal must return no certificate")
			}
		})
	}
}

func TestNamesInZone(t *testing.T) {
	for name, want := range map[string]bool{
		"lab.example": true, "www.lab.example": true, "a.b.lab.example": true, "*.lab.example": true,
		"WWW.Lab.Example": true, "lab.example.": false, "example": false, "xlab.example": false,
		"lab.example.org": false, "": false,
	} {
		if got := labRules.inZone(name); got != want {
			t.Errorf("inZone(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLifetimeCap(t *testing.T) {
	ca := newCert(t, "Lab Issuer CA", "ecdsa", true, nil, nil)
	a := mustLoad(t, ca.pem, keyPEM(t, ca.key, false), nil)
	for _, tc := range []struct {
		name      string
		asked     time.Duration
		cap, want time.Duration
	}{
		{"none asked, so 90 days", 0, 24 * time.Hour, 24 * time.Hour},
		{"a year", 8760 * time.Hour, 24 * time.Hour, 24 * time.Hour},
		{"one second over", 24*time.Hour + time.Second, 24 * time.Hour, 24 * time.Hour},
		{"exactly the cap", 24 * time.Hour, 24 * time.Hour, 24 * time.Hour},
		{"under the cap", 2 * time.Hour, 24 * time.Hour, 2 * time.Hour},
		{"another cap, by flag", 2 * time.Hour, time.Hour, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, _ := request(t, ask{dns: []string{"www.lab.example"}, duration: tc.asked})
			before := time.Now()
			_, leaf, err := labSigner{rules: rules{zone: "lab.example", maxLifetime: tc.cap}}.issue(a, cr)
			if err != nil {
				t.Fatalf("a long lifetime is shortened, not refused: %v", err)
			}
			if got := leaf.NotAfter.Sub(leaf.NotBefore); got != tc.want {
				t.Errorf("lifetime: got %s, want %s", got, tc.want)
			}
			if leaf.NotBefore.Before(before.Add(-2*time.Second)) || leaf.NotBefore.After(time.Now().Add(time.Second)) {
				t.Errorf("notBefore %s is not now", leaf.NotBefore)
			}
		})
	}
}

func TestOtherZone(t *testing.T) {
	ca := newCert(t, "Lab Issuer CA", "ecdsa", true, nil, nil)
	a := mustLoad(t, ca.pem, keyPEM(t, ca.key, false), nil)
	s := labSigner{rules: rules{zone: "team-a.test", maxLifetime: time.Hour}}
	cr, _ := request(t, ask{dns: []string{"www.team-a.test"}})
	if _, _, err := s.issue(a, cr); err != nil {
		t.Errorf("a name in the zone the flag set: %v", err)
	}
	cr, _ = request(t, ask{dns: []string{"www.lab.example"}})
	if _, _, err := s.issue(a, cr); err == nil || err.Error() != `house rule: "www.lab.example" is outside team-a.test` {
		t.Errorf("a name outside the zone the flag set: got %v", err)
	}
}

// A CSR may carry kinds of name Go does not parse. This one has a DNS name and an otherName (a
// Microsoft user principal name); the certificate must carry the DNS name and nothing else.
func TestUnparsedNamesAreDropped(t *testing.T) {
	upn := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 20, 2, 3}
	value, err := asn1.MarshalWithParams("admin@corp.example", "utf8,explicit,tag:0")
	if err != nil {
		t.Fatal(err)
	}
	oid, err := asn1.Marshal(upn)
	if err != nil {
		t.Fatal(err)
	}
	names, err := asn1.Marshal([]asn1.RawValue{
		{Class: asn1.ClassContextSpecific, Tag: 2, Bytes: []byte("www.lab.example")},
		{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: append(oid, value...)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ca := newCert(t, "Lab Issuer CA", "ecdsa", true, nil, nil)
	a := mustLoad(t, ca.pem, keyPEM(t, ca.key, false), nil)
	cr, _ := request(t, ask{extra: []pkix.Extension{{Id: oidSubjectAltName, Value: names}}})
	details, err := cr.GetCertificateDetails()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(details.CSR, []byte("CERTIFICATE REQUEST")) {
		t.Fatal("no CSR in the request")
	}
	_, leaf, err := labSigner{rules: labRules}.issue(a, cr)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !slices.Equal(leaf.DNSNames, []string{"www.lab.example"}) {
		t.Errorf("DNS names: got %v", leaf.DNSNames)
	}
	if bytes.Contains(leaf.Raw, []byte("admin@corp.example")) {
		t.Errorf("the certificate carries the otherName the CSR asked for")
	}
}

func TestBadRequests(t *testing.T) {
	ca := newCert(t, "Lab Issuer CA", "ecdsa", true, nil, nil)
	a := mustLoad(t, ca.pem, keyPEM(t, ca.key, false), nil)
	s := labSigner{rules: labRules}
	good, _ := request(t, ask{dns: []string{"www.lab.example"}})
	details, _ := good.GetCertificateDetails()

	// One flipped bit in the signed part: the CSR's own signature no longer holds.
	block, _ := pem.Decode(details.CSR)
	tampered := bytes.Clone(block.Bytes)
	at := bytes.Index(tampered, []byte("www.lab.example"))
	tampered[at] ^= 1

	for name, csr := range map[string][]byte{
		"junk":            []byte("not a CSR"),
		"empty":           nil,
		"a bad signature": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered}),
	} {
		t.Run(name, func(t *testing.T) {
			cr := signer.CertificateRequestObjectFromCertificateRequest(&cmapi.CertificateRequest{
				Spec: cmapi.CertificateRequestSpec{Request: csr},
			})
			_, _, err := s.issue(a, cr)
			if !errors.As(err, &signer.PermanentError{}) {
				t.Errorf("want a signer.PermanentError, got %v", err)
			}
		})
	}
	t.Run("a usage cert-manager does not know", func(t *testing.T) {
		cr, _ := request(t, ask{dns: []string{"www.lab.example"}, usages: []cmapi.KeyUsage{"sign everything"}})
		_, _, err := s.issue(a, cr)
		if !errors.As(err, &signer.PermanentError{}) {
			t.Errorf("want a signer.PermanentError, got %v", err)
		}
	})
}

func TestLoadAuthority(t *testing.T) {
	ca := newCert(t, "Lab Issuer CA", "ecdsa", true, nil, nil)
	other := newCert(t, "Another CA", "ecdsa", true, nil, nil)
	rsaCA := newCert(t, "RSA CA", "rsa", true, nil, nil)
	leaf := newCert(t, "www.lab.example", "ecdsa", false, ca, nil)
	noSign := newCert(t, "CA That May Not Sign", "ecdsa", true, nil, func(c *x509.Certificate) {
		c.KeyUsage = x509.KeyUsageDigitalSignature
	})
	caKey := keyPEM(t, ca.key, false)
	junkKey := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("junk")})

	for _, tc := range []struct {
		name         string
		crt, key, ca []byte
		want         string
	}{
		{"a key from another CA", ca.pem, keyPEM(t, other.key, false), nil,
			"tls.key is not the key of the certificate in tls.crt"},
		{"a key of another algorithm", ca.pem, keyPEM(t, rsaCA.key, false), nil,
			"tls.key is not the key of the certificate in tls.crt"},
		{"a certificate that is not a CA", leaf.pem, keyPEM(t, leaf.key, false), nil,
			`tls.crt is not a CA certificate (subject "CN=www.lab.example")`},
		{"a CA without cert sign", noSign.pem, keyPEM(t, noSign.key, false), nil,
			`tls.crt is a CA without the key usage "cert sign" (subject "CN=CA That May Not Sign")`},
		{"junk in tls.crt", []byte("not PEM at all"), caKey, nil,
			"tls.crt: no PEM certificate found"},
		{"an empty tls.crt", nil, caKey, nil,
			"tls.crt: no PEM certificate found"},
		{"PEM that is not a certificate", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}), caKey, nil,
			"tls.crt: x509: malformed certificate"},
		{"a key in tls.crt", caKey, caKey, nil,
			`tls.crt: a PEM block of type "EC PRIVATE KEY" where a certificate belongs`},
		{"text after the certificate", append(bytes.Clone(ca.pem), "oops\n"...), caKey, nil,
			"tls.crt: text after the last PEM block"},
		{"junk in tls.key", ca.pem, []byte("not PEM at all"), nil,
			"tls.key: no PEM block found"},
		{"an empty tls.key", ca.pem, nil, nil,
			"tls.key: no PEM block found"},
		{"PEM that is not a key", ca.pem, junkKey, nil,
			`tls.key: not a private key this issuer can read (PEM type "EC PRIVATE KEY")`},
		{"a certificate in tls.key", ca.pem, ca.pem, nil,
			`tls.key: a PEM block of type "CERTIFICATE" where a private key belongs`},
		{"junk in ca.crt", ca.pem, caKey, []byte("not PEM at all"),
			"ca.crt: no PEM certificate found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := loadAuthority(tc.crt, tc.key, tc.ca)
			if err == nil || a != nil {
				t.Fatalf("loaded, want an error")
			}
			if err.Error() != tc.want {
				t.Errorf("message:\n got %s\nwant %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY-----") {
				t.Errorf("the error carries a PEM block")
			}
		})
	}

	t.Run("a chain in tls.crt", func(t *testing.T) {
		root := newCert(t, "Lab Root", "ecdsa", true, nil, nil)
		inter := newCert(t, "Lab Issuer CA", "ecdsa", true, root, nil)
		a := mustLoad(t, append(bytes.Clone(inter.pem), root.pem...), keyPEM(t, inter.key, false), nil)
		if !a.cert.Equal(inter.cert) {
			t.Errorf("the CA is the first certificate of tls.crt")
		}
		if n := len(parseAll(t, a.chain)); n != 2 {
			t.Errorf("chain: got %d certificates, want 2", n)
		}
		if !bytes.Equal(a.root, inter.pem) {
			t.Errorf("root: with no ca.crt, want the CA certificate itself")
		}
	})
	t.Run("an empty ca.crt is no ca.crt", func(t *testing.T) {
		a := mustLoad(t, ca.pem, caKey, []byte("\n"))
		if !bytes.Equal(a.root, ca.pem) {
			t.Errorf("root: want the CA certificate")
		}
	})
}

func TestDeepCopy(t *testing.T) {
	in := &LabIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "demo", Labels: map[string]string{"a": "b"}},
		Spec:       LabIssuerSpec{SecretName: "lab-issuer-ca"},
	}
	in.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	out := in.DeepCopyObject().(*LabIssuer)
	out.Labels["a"] = "changed"
	out.Status.Conditions[0].Status = metav1.ConditionFalse
	if in.Labels["a"] != "b" || in.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("the copy shares memory with the original")
	}
	if out.Spec.SecretName != "lab-issuer-ca" || out.Name != "lab" {
		t.Errorf("the copy lost fields: %+v", out)
	}

	list := &LabClusterIssuerList{Items: []LabClusterIssuer{LabClusterIssuer(*in)}}
	copied := list.DeepCopyObject().(*LabClusterIssuerList)
	copied.Items[0].Labels["a"] = "changed"
	if list.Items[0].Labels["a"] != "b" {
		t.Errorf("the list copy shares memory with the original")
	}
	if (&LabIssuer{}).GetIssuerTypeIdentifier() != "labissuers.issuer.lab.example" ||
		(&LabClusterIssuer{}).GetIssuerTypeIdentifier() != "labclusterissuers.issuer.lab.example" {
		t.Errorf("the issuer type identifiers must match deploy/approve-rbac.yaml")
	}
}
