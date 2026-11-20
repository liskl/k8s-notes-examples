// cert-manager-demo is Field Guide 09's demo image: one binary, standard library only, that does the
// three things the book needs done with a certificate.
//
//	cert-manager-demo serve    an HTTPS server that says which certificate it served and who called
//	cert-manager-demo call     a client that calls such a server, once or on a timer
//	cert-manager-demo show     what is in a PEM file, one certificate at a time
//	cert-manager-demo version
//
// serve and call read their certificate, key and CA files again for every connection, so a Secret that
// cert-manager renews is used with no restart. Nothing here ever prints key material: show skips a
// private key block without reading it.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// version is set by the build: -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = `usage: cert-manager-demo <command> [flags]

  serve    serve HTTPS from -cert and -key; say which certificate was served and who called
  call     call an HTTPS server, once or every -every; print one line per call
  show     print what is in a PEM file (or stdin), one certificate at a time
  version  print the version

"cert-manager-demo <command> -h" lists a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCmd(args[1:], stderr)
	case "call":
		return callCmd(args[1:], stdout, stderr)
	case "show":
		return showCmd(args[1:], stdin, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "cert-manager-demo: no such command: %s\n\n%s", args[0], usage)
	return 2
}

// ---- files ---------------------------------------------------------------------------------------

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func list(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}

func subject(c *x509.Certificate) string {
	if s := c.Subject.String(); s != "" {
		return s
	}
	return "(empty subject)"
}

func uris(c *x509.Certificate) []string {
	var out []string
	for _, u := range c.URIs {
		out = append(out, u.String())
	}
	return out
}

// keyPair reads a certificate and its key from two files, and parses the first certificate.
func keyPair(certFile, keyFile string) (*tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if pair.Leaf == nil {
		if pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &pair, nil
}

func pool(file string) (*x509.CertPool, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s: no certificate found", file)
	}
	return p, nil
}

// ---- serve ---------------------------------------------------------------------------------------

type server struct {
	cert, key, ca string
	clientAuth    tls.ClientAuthType
	log           *log.Logger

	mu         sync.Mutex
	lastSerial string
}

// conn remembers which certificate its handshake served, so the handler can report that one and not
// whatever is in the files by the time the request arrives.
type conn struct {
	net.Conn
	served atomic.Pointer[x509.Certificate]
}

type listener struct{ net.Listener }

func (l listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &conn{Conn: c}, nil
}

type connKey struct{}

// config builds the TLS settings for one handshake from the files as they are now. This is the whole
// trick of serving a renewed certificate with no restart: the kubelet swaps the files of a mounted
// Secret, and the next handshake reads the new ones.
func (s *server) config(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	pair, err := keyPair(s.cert, s.key)
	if err != nil {
		s.log.Printf("handshake: cannot load the certificate: %v", err)
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{*pair},
		ClientAuth:   s.clientAuth,
	}
	if s.clientAuth != tls.NoClientCert {
		if cfg.ClientCAs, err = pool(s.ca); err != nil {
			s.log.Printf("handshake: cannot load the CA: %v", err)
			return nil, err
		}
	}
	if c, ok := hello.Conn.(*conn); ok {
		c.served.Store(pair.Leaf)
	}
	s.mu.Lock()
	if serial := pair.Leaf.SerialNumber.Text(16); serial != s.lastSerial {
		s.log.Printf("serving: serial=%s notAfter=%s", serial, timestamp(pair.Leaf.NotAfter))
		s.lastSerial = serial
	}
	s.mu.Unlock()
	return cfg, nil
}

// handle answers every path with the same few lines: who called, then which certificate was served.
func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	who := "no certificate"
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		c := r.TLS.PeerCertificates[0]
		who = subject(c)
		fmt.Fprintf(&b, "client: %s\n", who)
		fmt.Fprintf(&b, "client dns names: %s\n", list(c.DNSNames))
		fmt.Fprintf(&b, "client uris: %s\n", list(uris(c)))
		fmt.Fprintf(&b, "client issuer: %s\n", c.Issuer.String())
	} else {
		b.WriteString("client: no certificate\n")
	}
	if c, ok := r.Context().Value(connKey{}).(*conn); ok {
		if leaf := c.served.Load(); leaf != nil {
			fmt.Fprintf(&b, "server: %s\n", subject(leaf))
			fmt.Fprintf(&b, "server dns names: %s\n", list(leaf.DNSNames))
			fmt.Fprintf(&b, "server issuer: %s\n", leaf.Issuer.String())
			fmt.Fprintf(&b, "server serial: %s\n", leaf.SerialNumber.Text(16))
			fmt.Fprintf(&b, "server not after: %s\n", timestamp(leaf.NotAfter))
		}
	}
	s.log.Printf("request: %s %s client: %s", r.Method, r.URL.Path, who)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, b.String())
}

// start serves HTTPS on l until the returned server is shut down.
func (s *server) start(l net.Listener) *http.Server {
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: s.config},
		ErrorLog:          s.log, // handshake failures land here, one line each
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if t, ok := c.(*tls.Conn); ok {
				return context.WithValue(ctx, connKey{}, t.NetConn())
			}
			return ctx
		},
	}
	go func() {
		if err := srv.ServeTLS(listener{l}, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Printf("serve: %v", err)
		}
	}()
	return srv
}

func clientAuthType(name string) (tls.ClientAuthType, error) {
	switch name {
	case "none":
		return tls.NoClientCert, nil
	case "request":
		// A client may come without a certificate; one that brings a certificate must bring a good one.
		return tls.VerifyClientCertIfGiven, nil
	case "require":
		return tls.RequireAndVerifyClientCert, nil
	}
	return 0, fmt.Errorf("-client-auth must be none, request or require, not %q", name)
}

func serveCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8443", "HTTPS listen address")
	healthAddr := fs.String("health-addr", ":8080", "plain HTTP listen address for /healthz")
	cert := fs.String("cert", "", "the server's certificate file, PEM (tls.crt)")
	key := fs.String("key", "", "the server's private key file, PEM (tls.key)")
	ca := fs.String("ca", "", "the CA file client certificates are checked against, PEM (ca.crt)")
	auth := fs.String("client-auth", "require", "client certificates: none, request or require")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	logger := log.New(stderr, "", 0)
	clientAuth, err := clientAuthType(*auth)
	switch {
	case err != nil:
		logger.Printf("serve: %v", err)
		return 2
	case *cert == "" || *key == "":
		logger.Print("serve: -cert and -key are required")
		return 2
	case clientAuth != tls.NoClientCert && *ca == "":
		logger.Printf("serve: -client-auth %s needs -ca", *auth)
		return 2
	}
	s := &server{cert: *cert, key: *key, ca: *ca, clientAuth: clientAuth, log: logger}
	// Fail at the start if the files are wrong now. Later on, a bad file fails one handshake at a time.
	if _, err := s.config(&tls.ClientHelloInfo{}); err != nil {
		return 1
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Printf("serve: %v", err)
		return 1
	}
	health := &http.Server{Addr: *healthAddr, ReadHeaderTimeout: 10 * time.Second, Handler: healthHandler()}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("serve: health: %v", err)
			os.Exit(1)
		}
	}()
	srv := s.start(l)
	logger.Printf("serve: version=%s https=%s health=%s client-auth=%s", version, *addr, *healthAddr, *auth)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	health.Shutdown(ctx)
	return 0
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

// ---- call ----------------------------------------------------------------------------------------

type caller struct {
	url, cert, key, ca, serverName string
	timeout                        time.Duration
}

// once makes one call and returns the line to print. The files are read here, on every call, and the
// connection is not kept, so each call makes a new handshake and sees the certificate served now.
func (c caller) once() (string, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.serverName}
	if c.ca != "" {
		roots, err := pool(c.ca)
		if err != nil {
			return "", err
		}
		cfg.RootCAs = roots
	}
	if c.cert != "" {
		pair, err := keyPair(c.cert, c.key)
		if err != nil {
			return "", err
		}
		// Send this certificate whenever the server asks for one. Set as cfg.Certificates instead, Go
		// would first compare its issuer with the CA names the server sends, and send nothing when they
		// differ: the server would then say "certificate required", not what is wrong with this one.
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return pair, nil }
	}
	client := &http.Client{
		Timeout:   c.timeout,
		Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true},
	}
	resp, err := client.Get(c.url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(string(body), "\n")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("the server answered %s", resp.Status)
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return "", errors.New("the answer did not come over TLS")
	}
	leaf := resp.TLS.PeerCertificates[0]
	return fmt.Sprintf("ok serial=%s issuer=%q notAfter=%s body=%q",
		leaf.SerialNumber.Text(16), leaf.Issuer.String(), timestamp(leaf.NotAfter), first), nil
}

func callCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c caller
	fs.StringVar(&c.url, "url", "", "the URL to GET, https://...")
	fs.StringVar(&c.cert, "cert", "", "the client's certificate file, PEM (optional: without it, no certificate is sent)")
	fs.StringVar(&c.key, "key", "", "the client's private key file, PEM (with -cert)")
	fs.StringVar(&c.ca, "ca", "", "the CA file the server is checked against, PEM (default: the system's roots)")
	fs.StringVar(&c.serverName, "servername", "", "the name to check the server's certificate for (default: the URL's host)")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Second, "how long one call may take")
	every := fs.Duration("every", 0, "call again after this long, for ever (0: call once, and exit 1 if it fails)")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	switch {
	case c.url == "":
		fmt.Fprintln(stderr, "call: -url is required")
		return 2
	case (c.cert == "") != (c.key == ""):
		fmt.Fprintln(stderr, "call: -cert and -key go together")
		return 2
	}
	for {
		line, err := c.once()
		if err != nil {
			// Go's own words, unchanged: the book prints them.
			fmt.Fprintf(stdout, "error: %v\n", err)
		} else {
			fmt.Fprintln(stdout, line)
		}
		if *every <= 0 {
			if err != nil {
				return 1
			}
			return 0
		}
		time.Sleep(*every)
	}
}

// ---- show ----------------------------------------------------------------------------------------

var keyUsages = []struct {
	bit  x509.KeyUsage
	name string
}{ // the names cert-manager uses in a Certificate's spec.usages
	{x509.KeyUsageDigitalSignature, "digital signature"},
	{x509.KeyUsageContentCommitment, "content commitment"},
	{x509.KeyUsageKeyEncipherment, "key encipherment"},
	{x509.KeyUsageDataEncipherment, "data encipherment"},
	{x509.KeyUsageKeyAgreement, "key agreement"},
	{x509.KeyUsageCertSign, "cert sign"},
	{x509.KeyUsageCRLSign, "crl sign"},
	{x509.KeyUsageEncipherOnly, "encipher only"},
	{x509.KeyUsageDecipherOnly, "decipher only"},
}

var extKeyUsages = map[x509.ExtKeyUsage]string{
	x509.ExtKeyUsageAny:             "any",
	x509.ExtKeyUsageServerAuth:      "server auth",
	x509.ExtKeyUsageClientAuth:      "client auth",
	x509.ExtKeyUsageCodeSigning:     "code signing",
	x509.ExtKeyUsageEmailProtection: "email protection",
	x509.ExtKeyUsageTimeStamping:    "timestamping",
	x509.ExtKeyUsageOCSPSigning:     "ocsp signing",
}

func keyUsageNames(c *x509.Certificate) []string {
	var out []string
	for _, u := range keyUsages {
		if c.KeyUsage&u.bit != 0 {
			out = append(out, u.name)
		}
	}
	return out
}

func extKeyUsageNames(c *x509.Certificate) []string {
	var out []string
	for _, u := range c.ExtKeyUsage {
		if name, ok := extKeyUsages[u]; ok {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("unknown (%d)", int(u)))
		}
	}
	return out
}

func publicKey(c *x509.Certificate) string {
	switch k := c.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d bits", k.N.BitLen())
	case *ecdsa.PublicKey:
		return fmt.Sprintf("ECDSA %d bits (%s)", k.Curve.Params().BitSize, k.Curve.Params().Name)
	case ed25519.PublicKey:
		return fmt.Sprintf("Ed25519 %d bits", len(k)*8)
	}
	return c.PublicKeyAlgorithm.String()
}

// show prints every certificate of a PEM stream, in order, and how many there were. With a CA file it
// also says whether the first certificate verifies against it, with the rest as intermediates.
//
// A private key block is never parsed and never printed: the block's type is all that is looked at.
func show(in io.Reader, caFile string, out io.Writer) error {
	rest, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	var certs []*x509.Certificate
	blocks := 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		blocks++
		switch {
		case strings.Contains(b.Type, "PRIVATE KEY"):
			fmt.Fprintln(out, "skipped: a private key block")
			continue
		case b.Type != "CERTIFICATE":
			fmt.Fprintf(out, "skipped: a block of type %q\n", b.Type)
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return fmt.Errorf("certificate block %d: %w", len(certs)+1, err)
		}
		certs = append(certs, c)
		var ips []string
		for _, ip := range c.IPAddresses {
			ips = append(ips, ip.String())
		}
		fmt.Fprintf(out, "certificate %d\n", len(certs))
		fmt.Fprintf(out, "  subject:     %s\n", subject(c))
		fmt.Fprintf(out, "  issuer:      %s\n", c.Issuer.String())
		fmt.Fprintf(out, "  dns names:   %s\n", list(c.DNSNames))
		fmt.Fprintf(out, "  ips:         %s\n", list(ips))
		fmt.Fprintf(out, "  uris:        %s\n", list(uris(c)))
		fmt.Fprintf(out, "  not before:  %s\n", timestamp(c.NotBefore))
		fmt.Fprintf(out, "  not after:   %s\n", timestamp(c.NotAfter))
		fmt.Fprintf(out, "  serial:      %s\n", c.SerialNumber.Text(16))
		fmt.Fprintf(out, "  ca:          %t\n", c.BasicConstraintsValid && c.IsCA)
		fmt.Fprintf(out, "  key usage:   %s\n", list(keyUsageNames(c)))
		fmt.Fprintf(out, "  ext usage:   %s\n", list(extKeyUsageNames(c)))
		fmt.Fprintf(out, "  public key:  %s\n", publicKey(c))
	}
	if blocks == 0 {
		return errors.New("no PEM block found")
	}
	fmt.Fprintf(out, "chain: %d certificate(s)\n", len(certs))
	if caFile == "" {
		return nil
	}
	if len(certs) == 0 {
		return errors.New("-ca was given, but there is no certificate to verify")
	}
	roots, err := pool(caFile)
	if err != nil {
		return err
	}
	inters := x509.NewCertPool()
	for _, c := range certs[1:] {
		inters.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inters,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, // any purpose: this checks the chain, not the use
	}); err != nil {
		fmt.Fprintf(out, "verify: %v\n", err)
		return errNotVerified
	}
	fmt.Fprintln(out, "verify: ok")
	return nil
}

var errNotVerified = errors.New("not verified")

func showCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cert-manager-demo show [-ca file] [file]   (no file, or -, reads stdin)")
		fs.PrintDefaults()
	}
	ca := fs.String("ca", "", "also verify the first certificate against this CA file, with the rest as intermediates")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	in := stdin
	switch {
	case fs.NArg() > 1:
		fs.Usage()
		return 2
	case fs.NArg() == 1 && fs.Arg(0) != "-":
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "show: %v\n", err)
			return 1
		}
		defer f.Close()
		in = f
	}
	if err := show(in, *ca, stdout); err != nil {
		if !errors.Is(err, errNotVerified) { // that one is already printed, as the verify line
			fmt.Fprintf(stderr, "show: %v\n", err)
		}
		return 1
	}
	return 0
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
