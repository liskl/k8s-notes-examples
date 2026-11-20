package main

// Real TLS over loopback, with keys made in memory and written only to the test's own temporary folder.
// Nothing is faked: serve and call are the code the image runs.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type issued struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte
}

var serials struct {
	sync.Mutex
	next int64
}

// issue makes a certificate signed by parent, or by itself if parent is nil.
func issue(t *testing.T, parent *issued, tmpl x509.Certificate) *issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serials.Lock()
	serials.next++
	tmpl.SerialNumber = big.NewInt(0xabc000 + serials.next)
	serials.Unlock()
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	tmpl.BasicConstraintsValid = true
	if tmpl.IsCA {
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	} else {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	}
	signerCert, signerKey := &tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &issued{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

func ca(t *testing.T, cn string) *issued {
	return issue(t, nil, x509.Certificate{Subject: pkix.Name{CommonName: cn}, IsCA: true})
}

func serverCert(t *testing.T, parent *issued) *issued {
	return issue(t, parent, x509.Certificate{
		Subject:     pkix.Name{CommonName: "www.lab.example"},
		DNSNames:    []string{"www.lab.example", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
}

func clientCert(t *testing.T, parent *issued, cn string) *issued {
	id, _ := url.Parse("spiffe://lab.example/ns/demo/sa/" + cn)
	return issue(t, parent, x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		DNSNames:    []string{cn + ".demo.lab.example"},
		URIs:        []*url.URL{id},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

// write puts files in dir the way the kubelet updates a mounted Secret: whole files, swapped by rename.
func write(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		tmp := filepath.Join(dir, "."+name+".tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

type lab struct {
	dir  string
	url  string
	logs *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startServer writes the server's files and serves from them on a free loopback port.
func startServer(t *testing.T, auth string, srvCert, clientCA *issued) *lab {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, map[string][]byte{"tls.crt": srvCert.certPEM, "tls.key": srvCert.keyPEM, "ca.crt": clientCA.certPEM})
	clientAuth, err := clientAuthType(auth)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	s := &server{
		cert: filepath.Join(dir, "tls.crt"), key: filepath.Join(dir, "tls.key"), ca: filepath.Join(dir, "ca.crt"),
		clientAuth: clientAuth, log: log.New(logs, "", 0),
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := s.start(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &lab{dir: dir, url: "https://" + l.Addr().String() + "/", logs: logs}
}

// clientFiles writes what a client mounts: its certificate and key (if it has one) and the server's CA.
func clientFiles(t *testing.T, c, serverCA *issued) (cert, key, caFile string) {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, map[string][]byte{"ca.crt": serverCA.certPEM})
	caFile = filepath.Join(dir, "ca.crt")
	if c != nil {
		write(t, dir, map[string][]byte{"tls.crt": c.certPEM, "tls.key": c.keyPEM})
		cert, key = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	}
	return cert, key, caFile
}

func serial(c *issued) string { return c.cert.SerialNumber.Text(16) }

func TestMutualTLS(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	srv := serverCert(t, root)
	s := startServer(t, "require", srv, root)
	cert, key, caFile := clientFiles(t, clientCert(t, root, "web"), root)

	line, err := caller{url: s.url, cert: cert, key: key, ca: caFile, timeout: 5 * time.Second}.once()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	want := `ok serial=` + serial(srv) + ` issuer="CN=Lab Issuer CA" notAfter=` + timestamp(srv.cert.NotAfter) + ` body="client: CN=web"`
	if line != want {
		t.Errorf("line:\n got %s\nwant %s", line, want)
	}
	if !strings.Contains(s.logs.String(), "request: GET / client: CN=web") {
		t.Errorf("the server did not log the request:\n%s", s.logs)
	}
}

// The whole answer of serve, read with a plain Go client.
func TestServeBody(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	srv := serverCert(t, root)
	for _, tc := range []struct {
		auth   string
		client bool
		want   []string
	}{
		{"require", true, []string{
			"client: CN=web",
			"client dns names: web.demo.lab.example",
			"client uris: spiffe://lab.example/ns/demo/sa/web",
			"client issuer: CN=Lab Issuer CA",
		}},
		{"request", true, []string{"client: CN=web", "client dns names: web.demo.lab.example",
			"client uris: spiffe://lab.example/ns/demo/sa/web", "client issuer: CN=Lab Issuer CA"}},
		{"request", false, []string{"client: no certificate"}},
		{"none", false, []string{"client: no certificate"}},
		{"none", true, []string{"client: no certificate"}}, // never asked for, so never sent
	} {
		name := tc.auth + "/without"
		if tc.client {
			name = tc.auth + "/with"
		}
		t.Run(name, func(t *testing.T) {
			s := startServer(t, tc.auth, srv, root)
			var c *issued
			if tc.client {
				c = clientCert(t, root, "web")
			}
			cert, key, caFile := clientFiles(t, c, root)
			roots, err := pool(caFile)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &tls.Config{RootCAs: roots}
			if tc.client {
				pair, err := keyPair(cert, key)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Certificates = append(cfg.Certificates, *pair)
			}
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
			resp, err := client.Get(s.url + "any/path")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			want := strings.Join(append(tc.want,
				"server: CN=www.lab.example",
				"server dns names: www.lab.example, localhost",
				"server issuer: CN=Lab Issuer CA",
				"server serial: "+serial(srv),
				"server not after: "+timestamp(srv.cert.NotAfter),
			), "\n") + "\n"
			if string(body) != want {
				t.Errorf("body:\n got:\n%s\nwant:\n%s", body, want)
			}
		})
	}
}

func TestCallWithoutClientCertificate(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	s := startServer(t, "require", serverCert(t, root), root)
	_, _, caFile := clientFiles(t, nil, root)

	_, err := caller{url: s.url, ca: caFile, timeout: 5 * time.Second}.once()
	if err == nil {
		t.Fatal("the call went through with no client certificate")
	}
	if want := `Get "` + s.url + `": remote error: tls: certificate required`; err.Error() != want {
		t.Errorf("error:\n got %s\nwant %s", err, want)
	}
	waitFor(t, s.logs, "tls: client didn't provide a certificate")

	// The same through the command: one line that starts "error:", and exit code 1.
	var out, errOut bytes.Buffer
	if code := run([]string{"call", "-url", s.url, "-ca", caFile}, nil, &out, &errOut); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if want := `error: Get "` + s.url + `": remote error: tls: certificate required` + "\n"; out.String() != want {
		t.Errorf("output:\n got %s\nwant %s", out.String(), want)
	}
}

func TestClientCertificateFromAnotherCA(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	other := ca(t, "Some Other CA")
	s := startServer(t, "require", serverCert(t, root), root)
	cert, key, caFile := clientFiles(t, clientCert(t, other, "stranger"), root)

	_, err := caller{url: s.url, cert: cert, key: key, ca: caFile, timeout: 5 * time.Second}.once()
	if err == nil {
		t.Fatal("the call went through with a certificate from another CA")
	}
	if want := `Get "` + s.url + `": remote error: tls: unknown certificate authority`; err.Error() != want {
		t.Errorf("error:\n got %s\nwant %s", err, want)
	}
	waitFor(t, s.logs, "tls: failed to verify certificate: x509: certificate signed by unknown authority")

	// "request" lets a client come with no certificate, but not with a bad one.
	s = startServer(t, "request", serverCert(t, root), root)
	if _, err := (caller{url: s.url, cert: cert, key: key, ca: caFile, timeout: 5 * time.Second}).once(); err == nil ||
		!strings.Contains(err.Error(), "unknown certificate authority") {
		t.Errorf("-client-auth request took a certificate from another CA: %v", err)
	}
}

func TestServerNotTrustedByClient(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	other := ca(t, "Some Other CA")
	s := startServer(t, "none", serverCert(t, root), root)
	_, _, caFile := clientFiles(t, nil, other)

	_, err := caller{url: s.url, ca: caFile, timeout: 5 * time.Second}.once()
	if err == nil || !strings.Contains(err.Error(), "tls: failed to verify certificate: x509: certificate signed by unknown authority") {
		t.Errorf("error: got %v", err)
	}

	// The right CA, the wrong name.
	_, _, caFile = clientFiles(t, nil, root)
	_, err = caller{url: s.url, ca: caFile, serverName: "db.lab.example", timeout: 5 * time.Second}.once()
	if err == nil || !strings.Contains(err.Error(), "x509: certificate is valid for www.lab.example, localhost, not db.lab.example") {
		t.Errorf("error: got %v", err)
	}
	// And -servername with a name the certificate does carry.
	if _, err = (caller{url: s.url, ca: caFile, serverName: "www.lab.example", timeout: 5 * time.Second}).once(); err != nil {
		t.Errorf("-servername www.lab.example: %v", err)
	}
}

// A renewal: new files, the same process. The next call sees the new serial.
func TestRenewalWithoutRestart(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	first := serverCert(t, root)
	s := startServer(t, "require", first, root)
	cert, key, caFile := clientFiles(t, clientCert(t, root, "web"), root)
	c := caller{url: s.url, cert: cert, key: key, ca: caFile, timeout: 5 * time.Second}

	line, err := c.once()
	if err != nil || !strings.HasPrefix(line, "ok serial="+serial(first)+" ") {
		t.Fatalf("before the renewal: %q, %v", line, err)
	}

	second := serverCert(t, root)
	if serial(second) == serial(first) {
		t.Fatal("the test made the same serial twice")
	}
	write(t, s.dir, map[string][]byte{"tls.crt": second.certPEM, "tls.key": second.keyPEM})
	line, err = c.once()
	if err != nil || !strings.HasPrefix(line, "ok serial="+serial(second)+" ") {
		t.Fatalf("after the renewal: %q, %v (want serial %s)", line, err, serial(second))
	}
	for _, want := range []string{"serving: serial=" + serial(first), "serving: serial=" + serial(second)} {
		if !strings.Contains(s.logs.String(), want) {
			t.Errorf("the server did not log %q:\n%s", want, s.logs)
		}
	}

	// The client's side renews the same way: a new client certificate is sent on the next call.
	renewed := clientCert(t, root, "web-renewed")
	write(t, filepath.Dir(cert), map[string][]byte{"tls.crt": renewed.certPEM, "tls.key": renewed.keyPEM})
	if line, err = c.once(); err != nil || !strings.HasSuffix(line, `body="client: CN=web-renewed"`) {
		t.Errorf("after the client's renewal: %q, %v", line, err)
	}

	// And the CA file: once the server's ca.crt names another CA, the old client is turned away.
	write(t, s.dir, map[string][]byte{"ca.crt": ca(t, "Rotated CA").certPEM})
	if _, err = c.once(); err == nil || !strings.Contains(err.Error(), "unknown certificate authority") {
		t.Errorf("after the CA changed: got %v", err)
	}
}

// A certificate and a key that do not belong together fail the handshake, not the process.
func TestBrokenFilesFailTheHandshakeOnly(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	good := serverCert(t, root)
	s := startServer(t, "none", good, root)
	_, _, caFile := clientFiles(t, nil, root)
	c := caller{url: s.url, ca: caFile, timeout: 5 * time.Second}

	write(t, s.dir, map[string][]byte{"tls.key": serverCert(t, root).keyPEM})
	if _, err := c.once(); err == nil {
		t.Fatal("the call went through with a mismatched pair")
	}
	waitFor(t, s.logs, "handshake: cannot load the certificate: tls: private key does not match public key")
	if strings.Contains(s.logs.String(), "PRIVATE KEY") {
		t.Errorf("the log carries key material")
	}

	write(t, s.dir, map[string][]byte{"tls.key": good.keyPEM})
	if _, err := c.once(); err != nil {
		t.Errorf("after the files were put right: %v", err)
	}
}

func TestCallEvery(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	s := startServer(t, "none", serverCert(t, root), root)
	_, _, caFile := clientFiles(t, nil, root)
	out := &syncBuffer{}
	go run([]string{"call", "-url", s.url, "-ca", caFile, "-every", "20ms"}, nil, out, io.Discard)
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(out.String(), "ok serial=") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than three calls in five seconds:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCallStatus(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	dir := t.TempDir()
	write(t, dir, map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})})
	_, err := caller{url: ts.URL, ca: filepath.Join(dir, "ca.crt"), timeout: 5 * time.Second}.once()
	if err == nil || err.Error() != "the server answered 404 Not Found" {
		t.Errorf("error: got %v", err)
	}
}

func TestShowBundle(t *testing.T) {
	root := ca(t, "Lab Root")
	inter := issue(t, root, x509.Certificate{Subject: pkix.Name{CommonName: "Lab Issuer CA"}, IsCA: true})
	leaf := issue(t, inter, x509.Certificate{
		Subject:     pkix.Name{CommonName: "www.lab.example", Organization: []string{"Lab"}},
		DNSNames:    []string{"www.lab.example", "api.lab.example"},
		IPAddresses: []net.IP{net.ParseIP("10.96.0.10")},
		URIs:        []*url.URL{{Scheme: "spiffe", Host: "lab.example", Path: "/ns/demo/sa/web"}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	})
	dir := t.TempDir()
	write(t, dir, map[string][]byte{
		"tls.crt":   append(bytes.Clone(leaf.certPEM), inter.certPEM...),
		"ca.crt":    root.certPEM,
		"other.crt": ca(t, "Some Other CA").certPEM,
	})

	var out, errOut bytes.Buffer
	code := run([]string{"show", "-ca", filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt")}, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errOut.String())
	}
	want := `certificate 1
  subject:     CN=www.lab.example,O=Lab
  issuer:      CN=Lab Issuer CA
  dns names:   www.lab.example, api.lab.example
  ips:         10.96.0.10
  uris:        spiffe://lab.example/ns/demo/sa/web
  not before:  ` + timestamp(leaf.cert.NotBefore) + `
  not after:   ` + timestamp(leaf.cert.NotAfter) + `
  serial:      ` + serial(leaf) + `
  ca:          false
  key usage:   digital signature
  ext usage:   server auth, client auth
  public key:  ECDSA 256 bits (P-256)
certificate 2
  subject:     CN=Lab Issuer CA
  issuer:      CN=Lab Root
  dns names:   -
  ips:         -
  uris:        -
  not before:  ` + timestamp(inter.cert.NotBefore) + `
  not after:   ` + timestamp(inter.cert.NotAfter) + `
  serial:      ` + serial(inter) + `
  ca:          true
  key usage:   cert sign, crl sign
  ext usage:   -
  public key:  ECDSA 256 bits (P-256)
chain: 2 certificate(s)
verify: ok
`
	if out.String() != want {
		t.Errorf("output:\n got:\n%s\nwant:\n%s", out.String(), want)
	}
	if !strings.HasSuffix(timestamp(leaf.cert.NotAfter), "Z") {
		t.Errorf("times must be UTC: %s", timestamp(leaf.cert.NotAfter))
	}

	// The same bundle from stdin, against a CA that did not sign it: the verify line is Go's error.
	out.Reset()
	code = run([]string{"show", "-ca", filepath.Join(dir, "other.crt")}, bytes.NewReader(append(bytes.Clone(leaf.certPEM), inter.certPEM...)), &out, &errOut)
	if code != 1 {
		t.Errorf("exit code %d, want 1 when the chain does not verify", code)
	}
	if !strings.HasSuffix(out.String(), "chain: 2 certificate(s)\nverify: x509: certificate signed by unknown authority\n") {
		t.Errorf("output ends:\n%s", out.String())
	}

	// The leaf alone verifies against the root only with its intermediate.
	out.Reset()
	if code = run([]string{"show", "-ca", filepath.Join(dir, "ca.crt"), "-"}, bytes.NewReader(leaf.certPEM), &out, &errOut); code != 1 ||
		!strings.Contains(out.String(), "chain: 1 certificate(s)\nverify: x509: certificate signed by unknown authority") {
		t.Errorf("a leaf without its intermediate: exit %d, output:\n%s", code, out.String())
	}
}

func TestShowKeyTypes(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(255), Subject: pkix.Name{CommonName: "rsa"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rsaKey.PublicKey, rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := show(bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "", &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  public key:  RSA 2048 bits\n", "  serial:      ff\n", "  key usage:   digital signature, key encipherment\n", "  ca:          false\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

// A Secret's three values in one stream, as "kubectl get secret -o json | jq" would hand them over: the
// key block is named and skipped, and not one byte of it reaches the output.
func TestShowSkipsPrivateKeys(t *testing.T) {
	root := ca(t, "Lab Issuer CA")
	leaf := serverCert(t, root)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(leaf.key)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string][]byte{
		"EC PRIVATE KEY":        leaf.keyPEM,
		"PRIVATE KEY":           pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
		"RSA PRIVATE KEY":       pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}),
		"ENCRYPTED PRIVATE KEY": pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: pkcs8}),
	}
	for typ, keyPEM := range keys {
		t.Run(typ, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run([]string{"show"}, bytes.NewReader(keyPEM), &out, &errOut); code != 0 {
				t.Fatalf("exit code %d: %s", code, errOut.String())
			}
			if want := "skipped: a private key block\nchain: 0 certificate(s)\n"; out.String() != want {
				t.Errorf("output:\n got %q\nwant %q", out.String(), want)
			}
			assertNoKeyMaterial(t, out.String()+errOut.String(), keyPEM)
		})
	}

	t.Run("between two certificates", func(t *testing.T) {
		in := bytes.Join([][]byte{leaf.certPEM, leaf.keyPEM, root.certPEM}, nil)
		var out, errOut bytes.Buffer
		if code := run([]string{"show"}, bytes.NewReader(in), &out, &errOut); code != 0 {
			t.Fatalf("exit code %d: %s", code, errOut.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if lines[0] != "certificate 1" || lines[13] != "skipped: a private key block" || lines[14] != "certificate 2" ||
			lines[len(lines)-1] != "chain: 2 certificate(s)" {
			t.Errorf("output:\n%s", out.String())
		}
		assertNoKeyMaterial(t, out.String()+errOut.String(), leaf.keyPEM)
	})

	t.Run("other blocks and no blocks", func(t *testing.T) {
		var out, errOut bytes.Buffer
		csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("x")})
		if code := run([]string{"show"}, bytes.NewReader(csr), &out, &errOut); code != 0 ||
			out.String() != "skipped: a block of type \"CERTIFICATE REQUEST\"\nchain: 0 certificate(s)\n" {
			t.Errorf("a CSR block: exit %d, output %q", code, out.String())
		}
		out.Reset()
		if code := run([]string{"show"}, strings.NewReader("hello\n"), &out, &errOut); code != 1 ||
			out.Len() != 0 || errOut.String() != "show: no PEM block found\n" {
			t.Errorf("no PEM: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
		}
	})
}

// assertNoKeyMaterial fails if any 16-character run of the key's base64 body shows up in the output.
func assertNoKeyMaterial(t *testing.T, output string, keyPEM []byte) {
	t.Helper()
	if strings.Contains(output, "-----BEGIN") {
		t.Errorf("the output holds a PEM block:\n%s", output)
	}
	for _, line := range strings.Split(string(keyPEM), "\n") {
		if strings.HasPrefix(line, "-----") || len(line) < 16 {
			continue
		}
		for i := 0; i+16 <= len(line); i += 8 {
			if strings.Contains(output, line[i:i+16]) {
				t.Fatalf("the output holds part of the key")
			}
		}
	}
}

func TestCommandLine(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"version"}, 0, "dev\n", ""},
		{nil, 2, "", "usage: cert-manager-demo <command> [flags]"},
		{[]string{"frobnicate"}, 2, "", "cert-manager-demo: no such command: frobnicate"},
		{[]string{"help"}, 0, "usage: cert-manager-demo <command> [flags]", ""},
		{[]string{"call"}, 2, "", "call: -url is required"},
		{[]string{"call", "-url", "https://x.invalid", "-cert", "a"}, 2, "", "call: -cert and -key go together"},
		{[]string{"serve"}, 2, "", "serve: -cert and -key are required"},
		{[]string{"serve", "-cert", "a", "-key", "b"}, 2, "", "serve: -client-auth require needs -ca"},
		{[]string{"serve", "-cert", "a", "-key", "b", "-client-auth", "maybe"}, 2, "", `serve: -client-auth must be none, request or require, not "maybe"`},
		{[]string{"serve", "-cert", "/nonexistent/tls.crt", "-key", "/nonexistent/tls.key", "-client-auth", "none"}, 1, "",
			"handshake: cannot load the certificate: open /nonexistent/tls.crt: no such file or directory"},
		{[]string{"show", "/nonexistent/tls.crt"}, 1, "", "show: open /nonexistent/tls.crt: no such file or directory"},
		{[]string{"show", "a", "b"}, 2, "", "usage: cert-manager-demo show [-ca file] [file]"},
		{[]string{"serve", "-h"}, 0, "", "-client-auth"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &out, &errOut); code != tc.code {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tc.code, errOut.String())
			}
			if !strings.HasPrefix(out.String(), tc.stdout) || (tc.stdout == "" && out.Len() != 0) {
				t.Errorf("stdout: got %q, want it to start %q", out.String(), tc.stdout)
			}
			if !strings.Contains(errOut.String(), tc.stderr) || (tc.stderr == "" && errOut.Len() != 0) {
				t.Errorf("stderr: got %q, want %q in it", errOut.String(), tc.stderr)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	ts := httptest.NewServer(healthHandler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok\n" {
		t.Errorf("/healthz: %d %q", resp.StatusCode, body)
	}
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("the health port serves only /healthz: got %d for /", resp.StatusCode)
	}
}

// waitFor waits for the server's log to hold a line: the server logs a failed handshake after the
// client has already seen its side of it.
func waitFor(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the server never logged %q:\n%s", want, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
