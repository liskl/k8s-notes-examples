package main

// The parts of the issuer that need no cluster: reading a CA out of a Secret's bytes, holding a request
// to the house rules, and signing. signer.go wires them to issuer-lib; ca_test.go tests them with real
// keys made in memory.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/cert-manager/issuer-lib/controllers/signer"
)

// authority is a CA ready to sign: what a Secret of type kubernetes.io/tls holds, checked.
type authority struct {
	cert  *x509.Certificate
	key   crypto.Signer
	chain []byte // every certificate in tls.crt, the CA first, as PEM
	root  []byte // ca.crt if the Secret has one, else the CA certificate, as PEM
}

// loadAuthority checks the three values of a CA Secret and returns the CA. Each error says which value
// is wrong and why, and none of them carries key material.
//
// The last check, that tls.key is the key of the certificate in tls.crt, is one cert-manager's own CA
// issuer does not make: it reports Ready with a mismatched pair and only fails when it signs.
func loadAuthority(crt, key, caCrt []byte) (*authority, error) {
	certs, err := certificates(crt)
	if err != nil {
		return nil, fmt.Errorf("tls.crt: %w", err)
	}
	ca := certs[0]
	if !ca.BasicConstraintsValid || !ca.IsCA {
		return nil, fmt.Errorf("tls.crt is not a CA certificate (subject %q)", ca.Subject.String())
	}
	if ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("tls.crt is a CA without the key usage \"cert sign\" (subject %q)", ca.Subject.String())
	}
	k, err := privateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tls.key: %w", err)
	}
	pub, ok := ca.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(k.Public()) {
		return nil, errors.New("tls.key is not the key of the certificate in tls.crt")
	}
	a := &authority{cert: ca, key: k, chain: encode(certs...), root: encode(ca)}
	if len(bytes.TrimSpace(caCrt)) > 0 {
		roots, err := certificates(caCrt)
		if err != nil {
			return nil, fmt.Errorf("ca.crt: %w", err)
		}
		a.root = encode(roots...)
	}
	return a, nil
}

// certificates reads every CERTIFICATE block of a PEM file, in order. Anything else in the file is an
// error: a CA Secret's tls.crt and ca.crt hold certificates only.
func certificates(pemBytes []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := pemBytes
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a PEM block of type %q where a certificate belongs", b.Type)
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no PEM certificate found")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("text after the last PEM block")
	}
	return out, nil
}

// privateKey reads the one private key of a PEM file: PKCS#8 (what cert-manager writes for Ed25519, and
// for any key with privateKey.encoding PKCS8), PKCS#1 (RSA) or SEC 1 (ECDSA).
func privateKey(pemBytes []byte) (crypto.Signer, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("no PEM block found")
	}
	var k any
	var err error
	switch b.Type {
	case "PRIVATE KEY":
		k, err = x509.ParsePKCS8PrivateKey(b.Bytes)
	case "RSA PRIVATE KEY":
		k, err = x509.ParsePKCS1PrivateKey(b.Bytes)
	case "EC PRIVATE KEY":
		k, err = x509.ParseECPrivateKey(b.Bytes)
	default:
		return nil, fmt.Errorf("a PEM block of type %q where a private key belongs", b.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("not a private key this issuer can read (PEM type %q)", b.Type)
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("a %T cannot sign", k)
	}
	return s, nil
}

func encode(certs ...*x509.Certificate) []byte {
	var out bytes.Buffer
	for _, c := range certs {
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return out.Bytes()
}

// rules are the house rules: what this issuer agrees to sign, whoever asks.
type rules struct {
	zone        string        // rule 1: every name is the zone or a name under it
	maxLifetime time.Duration // rule 3: no certificate outlives this (rule 2, no CAs, has no setting)
}

// houseRule is the error of a request the rules refuse. Its text is what the reader sees on the
// CertificateRequest, after issuer-lib's "Failed permanently to sign CertificateRequest: ".
type houseRule struct{ msg string }

func (h houseRule) Error() string { return "house rule: " + h.msg }

var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// template holds a request to the rules and returns the certificate to sign: the CSR's public key,
// subject and DNS names, the usages the request names, and a lifetime no longer than the cap. Every
// error is final for the request: the same request will never pass, so the caller reports each as a
// signer.PermanentError.
func (r rules) template(details signer.CertificateDetails) (*x509.Certificate, error) {
	if details.IsCA {
		return nil, houseRule{"isCA is refused: this issuer signs leaf certificates only"}
	}
	// issuer-lib checks the CSR's own signature here, and that the usages and the CA flag inside the
	// CSR agree with the ones the CertificateRequest names.
	tmpl, err := details.CertificateTemplate()
	if err != nil {
		return nil, err
	}
	switch {
	case len(tmpl.IPAddresses) > 0:
		return nil, houseRule{fmt.Sprintf("the IP address %s is refused: DNS names in %s only", tmpl.IPAddresses[0], r.zone)}
	case len(tmpl.URIs) > 0:
		return nil, houseRule{fmt.Sprintf("the URI %q is refused: DNS names in %s only", tmpl.URIs[0].String(), r.zone)}
	case len(tmpl.EmailAddresses) > 0:
		return nil, houseRule{fmt.Sprintf("the email address %q is refused: DNS names in %s only", tmpl.EmailAddresses[0], r.zone)}
	case len(tmpl.DNSNames) == 0:
		return nil, houseRule{fmt.Sprintf("the request has no DNS name: it needs at least one in %s", r.zone)}
	}
	for _, n := range tmpl.DNSNames {
		if !r.inZone(n) {
			return nil, houseRule{fmt.Sprintf("%q is outside %s", n, r.zone)}
		}
	}
	// issuer-lib copies the CSR's subjectAltName extension as raw bytes, which may hold kinds of name
	// Go does not parse (an otherName, say). Drop the copy, so the certificate's names are built from
	// the DNS names just checked and nothing else.
	extra := tmpl.ExtraExtensions[:0]
	for _, e := range tmpl.ExtraExtensions {
		if !e.Id.Equal(oidSubjectAltName) {
			extra = append(extra, e)
		}
	}
	tmpl.ExtraExtensions = extra
	if tmpl.NotAfter.Sub(tmpl.NotBefore) > r.maxLifetime {
		tmpl.NotAfter = tmpl.NotBefore.Add(r.maxLifetime)
	}
	return tmpl, nil
}

// inZone: the zone itself, or a name that ends in a dot and the zone. DNS names compare without case.
func (r rules) inZone(name string) bool {
	name = strings.ToLower(name)
	return name == r.zone || strings.HasSuffix(name, "."+r.zone)
}

// sign signs the template and returns what cert-manager stores: tls.crt is the leaf and then the CA's
// own chain; ca.crt is the root (written only when the issuer runs with -set-ca).
func (a *authority) sign(tmpl *x509.Certificate) (signer.PEMBundle, *x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return signer.PEMBundle{}, nil, err
	}
	tmpl.SerialNumber = serial
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, tmpl.PublicKey, a.key)
	if err != nil {
		return signer.PEMBundle{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return signer.PEMBundle{}, nil, err
	}
	return signer.PEMBundle{ChainPEM: append(encode(leaf), a.chain...), CAPEM: a.root}, leaf, nil
}
