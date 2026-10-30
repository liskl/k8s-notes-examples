// spiffe-demo is Field Guide 04's one binary: a small workload that talks to the SPIFFE Workload API (the
// SPIRE agent's socket, from SPIFFE_ENDPOINT_SOCKET) with go-spiffe, and prints what it gets.
//
//	whoami                 its X.509-SVIDs (ID, serial, dates, issuer), its bundles; waits up to -wait
//	watch                  one line each time its SVID or bundle changes
//	serve -allow ID        an mTLS server that lets only that ID in (-allow-domain TD: anyone in TD)
//	call URL -expect ID    an mTLS client that talks only to a server with that SPIFFE ID
//	jwt -audience A        a JWT-SVID's header and claims
//	jwt-check -audience A  fetch a JWT-SVID, validate it by the Workload API and by an OIDC /keys URL
//	inspect FILE           a PEM file's certificates (- reads stdin), or a JWT file's header and claims
//	get URL                an HTTPS GET that trusts the trust domain's bundle (the OIDC provider's)
//
// It never prints a private key or a whole token: keys stay in memory, a PEM key block is named by its type
// only, and tokens are split in memory to print their header and claims.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc/status"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = "usage: spiffe-demo whoami|watch|serve|call|jwt|jwt-check|inspect|get|version [flags]"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "version":
		fmt.Println(version)
	case "whoami":
		err = whoami(ctx, args)
	case "watch":
		err = watch(ctx)
	case "serve":
		err = serve(ctx, args)
	case "call":
		err = call(ctx, args)
	case "jwt":
		err = jwt(ctx, args)
	case "jwt-check":
		err = jwtCheck(ctx, args)
	case "inspect":
		err = inspect(args)
	case "get":
		err = get(ctx, args)
	default:
		err = errors.New(usage)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// printSVID prints one X.509-SVID, a field a line, so no line runs long.
func printSVID(s *x509svid.SVID) {
	c := s.Certificates[0]
	fmt.Printf("spiffe id   %s\n", s.ID)
	if s.Hint != "" {
		fmt.Printf("hint        %s\n", s.Hint)
	}
	fmt.Printf("serial      %s\n", c.SerialNumber.Text(16))
	fmt.Printf("not before  %s\n", ts(c.NotBefore))
	fmt.Printf("not after   %s\n", ts(c.NotAfter))
	fmt.Printf("issuer      %s\n", c.Issuer.String())
}

func printBundles(set *x509bundle.Set) {
	bs := set.Bundles()
	sort.Slice(bs, func(i, j int) bool { return bs[i].TrustDomain().String() < bs[j].TrustDomain().String() })
	for _, b := range bs {
		fmt.Printf("bundle      %s, %d CA\n", b.TrustDomain(), len(b.X509Authorities()))
	}
}

// fetchX509 asks the Workload API until it answers or wait runs out. A Pod's entry reaches its agent a few
// seconds after the Pod starts, so the first answer can be "no identity issued".
func fetchX509(ctx context.Context, wait time.Duration) (*workloadapi.X509Context, error) {
	deadline := time.Now().Add(wait)
	for {
		xc, err := workloadapi.FetchX509Context(ctx)
		if err == nil || time.Now().After(deadline) {
			return xc, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
	}
}

func whoami(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	wait := fs.Duration("wait", 10*time.Second, "how long to wait for an identity")
	fs.Parse(args)
	xc, err := fetchX509(ctx, *wait)
	if err != nil {
		return err
	}
	for i, s := range xc.SVIDs {
		if i > 0 {
			fmt.Println()
		}
		printSVID(s)
	}
	printBundles(xc.Bundles)
	return nil
}

// watcher prints one line for each update the Workload API streams: the first fetch, a renewed SVID (new
// serial), or a bundle that changed (a CA prepared, activated or pruned; a federated bundle added).
type watcher struct{ serial, bundles string }

func (w *watcher) OnX509ContextUpdate(c *workloadapi.X509Context) {
	s := c.DefaultSVID()
	serial := s.Certificates[0].SerialNumber.Text(16)
	var parts []string
	for _, b := range c.Bundles.Bundles() {
		parts = append(parts, fmt.Sprintf("%s:%d", b.TrustDomain(), len(b.X509Authorities())))
	}
	sort.Strings(parts)
	bundles := strings.Join(parts, " ")
	what := "fetched"
	switch {
	case w.serial == "":
	case serial != w.serial:
		what = "renewed"
	case bundles != w.bundles:
		what = "bundle "
	default:
		what = "update "
	}
	w.serial, w.bundles = serial, bundles
	fmt.Printf("%s %s serial=%s notAfter=%s CAs=%s\n", time.Now().UTC().Format("15:04:05"), what,
		short(serial), s.Certificates[0].NotAfter.UTC().Format("15:04:05"), bundles)
}

func (w *watcher) OnX509ContextWatchError(err error) {
	log.Printf("watch: %v", err)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

func watch(ctx context.Context) error {
	err := workloadapi.WatchX509Context(ctx, &watcher{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	allow := fs.String("allow", "", "the one SPIFFE ID allowed to call")
	allowDomain := fs.String("allow-domain", "", "or: any SPIFFE ID in this trust domain")
	addr := fs.String("addr", ":8443", "listen address")
	fs.Parse(args)
	var ok func(spiffeid.ID) bool
	var rule string
	switch {
	case *allow != "":
		want, err := spiffeid.FromString(*allow)
		if err != nil {
			return err
		}
		ok, rule = func(id spiffeid.ID) bool { return id == want }, want.String()
	case *allowDomain != "":
		td, err := spiffeid.TrustDomainFromString(*allowDomain)
		if err != nil {
			return err
		}
		ok, rule = func(id spiffeid.ID) bool { return id.MemberOf(td) }, "any ID in "+td.String()
	default:
		return errors.New("serve -allow ID or -allow-domain TD")
	}
	src, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	if s, err := src.GetX509SVID(); err == nil {
		log.Printf("serving on %s as %s", *addr, s.ID)
		log.Printf("allowing %s", rule)
	}
	authz := func(id spiffeid.ID, _ [][]*x509.Certificate) error {
		if !ok(id) {
			log.Printf("refused %s", id)
			return fmt.Errorf("unexpected ID %q", id)
		}
		log.Printf("accepted %s", id)
		return nil
	}
	srv := &http.Server{Addr: *addr, TLSConfig: tlsconfig.MTLSServerConfig(src, src, authz),
		ErrorLog: log.New(io.Discard, "", 0), // the authorizer already logs each refusal
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			id, _ := peerID(r.TLS)
			fmt.Fprintf(rw, "hello %s\n", id)
		})}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func peerID(cs *tls.ConnectionState) (spiffeid.ID, error) {
	if cs == nil || len(cs.PeerCertificates) == 0 || len(cs.PeerCertificates[0].URIs) == 0 {
		return spiffeid.ID{}, errors.New("no peer SPIFFE ID")
	}
	return spiffeid.FromURI(cs.PeerCertificates[0].URIs[0])
}

func call(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	expect := fs.String("expect", "", "the SPIFFE ID the server must present")
	wait := fs.Duration("wait", 10*time.Second, "how long to wait for an identity")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 || *expect == "" {
		return errors.New("call URL -expect ID")
	}
	want, err := spiffeid.FromString(*expect)
	if err != nil {
		return err
	}
	if _, err := fetchX509(ctx, *wait); err != nil {
		return err
	}
	src, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeID(want))}}
	resp, err := cl.Get(pos[0])
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("%d %s", resp.StatusCode, b)
	return nil
}

// parseInterspersed lets flags come after the positional arguments (call URL -expect ID): the standard flag
// package stops at the first argument that is not a flag.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// claimsOf splits a compact JWT in memory and decodes its header and claims; the signature is dropped.
func claimsOf(tok string) (hdr, claims map[string]any, err error) {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return nil, nil, errors.New("not a JWT")
	}
	dec := func(s string, v *map[string]any) error {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	if err = dec(p[0], &hdr); err != nil {
		return
	}
	err = dec(p[1], &claims)
	return
}

func printClaims(hdr, claims map[string]any) {
	fmt.Printf("alg   %v\n", hdr["alg"])
	fmt.Printf("kid   %v\n", hdr["kid"])
	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := claims[k]
		if f, ok := v.(float64); ok && (k == "exp" || k == "iat" || k == "nbf") {
			v = ts(time.Unix(int64(f), 0))
		}
		fmt.Printf("%-5s %v\n", k, v)
	}
}

// fetchJWT waits for the workload's identity as fetchX509 does, then asks for a JWT-SVID for the audience.
func fetchJWT(ctx context.Context, aud string) (*jwtsvid.SVID, error) {
	if _, err := fetchX509(ctx, 10*time.Second); err != nil {
		return nil, err
	}
	return workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: aud})
}

func jwt(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jwt", flag.ExitOnError)
	aud := fs.String("audience", "", "the audience to ask for")
	fs.Parse(args)
	if *aud == "" {
		return errors.New("jwt -audience A")
	}
	svid, err := fetchJWT(ctx, *aud)
	if err != nil {
		return err
	}
	h, c, err := claimsOf(svid.Marshal())
	if err != nil {
		return err
	}
	printClaims(h, c)
	return nil
}

// jwt-check fetches a token for -audience, then validates it against -check-audience (the same by default):
// first through the Workload API (the agent checks it), then, with -keys, against the keys an OIDC discovery
// provider publishes. -wait sleeps between the two, to let a short-lived token expire. The token never leaves
// memory.
func jwtCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jwt-check", flag.ExitOnError)
	aud := fs.String("audience", "", "the audience to fetch a token for")
	checkAud := fs.String("check-audience", "", "the audience to check it against (default: -audience)")
	keysURL := fs.String("keys", "", "an OIDC discovery provider's /keys URL")
	wait := fs.Duration("wait", 0, "time to wait before checking")
	fs.Parse(args)
	if *aud == "" {
		return errors.New("jwt-check -audience A [-check-audience B] [-keys URL] [-wait D]")
	}
	if *checkAud == "" {
		*checkAud = *aud
	}
	svid, err := fetchJWT(ctx, *aud)
	if err != nil {
		return err
	}
	fmt.Printf("fetched     a token for %s, expires %s\n", *aud, ts(svid.Expiry))
	if *wait > 0 {
		fmt.Printf("waiting     %s\n", *wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*wait):
		}
	}
	tok := svid.Marshal()
	if s, err := workloadapi.ValidateJWTSVID(ctx, tok, *checkAud); err != nil {
		st := status.Convert(err) // the agent's gRPC status: its code and message, no "rpc error" prefix
		fmt.Printf("agent       refused (%s): %s\n", st.Code(), st.Message())
	} else {
		fmt.Printf("agent       valid: %s\n", s.ID)
	}
	if *keysURL == "" {
		return nil
	}
	td := svid.ID.TrustDomain()
	body, status, err := httpsGet(ctx, *keysURL, td, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s answered %s", *keysURL, http.StatusText(status))
	}
	keys, err := jwtbundle.Parse(td, body)
	if err != nil {
		return err
	}
	if s, err := jwtsvid.ParseAndValidate(tok, keys, []string{*checkAud}); err != nil {
		fmt.Printf("oidc keys   refused: %v\n", err)
	} else {
		fmt.Printf("oidc keys   valid: %s (keys: %d)\n", s.ID, len(keys.JWTAuthorities()))
	}
	return nil
}

// httpsGet trusts the trust domain's X.509 authorities as a plain CA pool: the OIDC discovery provider serves
// with an X.509-SVID whose DNS names are its Service's.
func httpsGet(ctx context.Context, url string, td spiffeid.TrustDomain, host string) ([]byte, int, error) {
	xc, err := fetchX509(ctx, 10*time.Second)
	if err != nil {
		return nil, 0, err
	}
	if td.IsZero() {
		td = xc.DefaultSVID().ID.TrustDomain()
	}
	b, err := xc.Bundles.GetX509BundleForTrustDomain(td)
	if err != nil {
		return nil, 0, err
	}
	pool := x509.NewCertPool()
	for _, c := range b.X509Authorities() {
		pool.AddCert(c)
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if host != "" {
		req.Host = host
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

// get prints an HTTPS answer's status and body; a JSON body is indented, so no line runs long.
func get(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	tdName := fs.String("trust-domain", "", "the trust domain whose bundle to trust (default: its own)")
	host := fs.String("host", "", "a Host header to send (default: the URL's host)")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		return errors.New("get URL [-trust-domain TD] [-host H]")
	}
	var td spiffeid.TrustDomain
	if *tdName != "" {
		var err error
		if td, err = spiffeid.TrustDomainFromString(*tdName); err != nil {
			return err
		}
	}
	body, status, err := httpsGet(ctx, pos[0], td, *host)
	if err != nil {
		return err
	}
	fmt.Printf("%d %s\n", status, http.StatusText(status))
	var out bytes.Buffer
	if json.Indent(&out, body, "", "  ") == nil {
		body = out.Bytes()
	}
	fmt.Println(strings.TrimRight(string(body), "\n"))
	return nil
}

// inspect prints a PEM file's certificates, a field a line, or a JWT file's header and claims. Any other PEM
// block (a private key) is named by its type only, never printed.
func inspect(args []string) error {
	if len(args) != 1 {
		return errors.New("inspect FILE (- for stdin)")
	}
	var raw []byte
	var err error
	if args[0] == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(args[0])
	}
	if err != nil {
		return err
	}
	if t := strings.TrimSpace(string(raw)); strings.Count(t, ".") == 2 && !strings.Contains(t, "-----") {
		h, c, err := claimsOf(t)
		if err != nil {
			return err
		}
		printClaims(h, c)
		return nil
	}
	rest, n := raw, 0
	for {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		n++
		if b.Type != "CERTIFICATE" {
			fmt.Printf("block %d     %s, not shown\n", n, b.Type)
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return err
		}
		kind := "certificate"
		if c.IsCA {
			kind = "CA certificate"
		}
		fmt.Printf("block %d     %s\n", n, kind)
		for _, u := range c.URIs {
			fmt.Printf("uri         %s\n", u)
		}
		fmt.Printf("serial      %s\n", c.SerialNumber.Text(16))
		fmt.Printf("not before  %s\n", ts(c.NotBefore))
		fmt.Printf("not after   %s\n", ts(c.NotAfter))
		fmt.Printf("subject     %s\n", c.Subject.String())
		fmt.Printf("issuer      %s\n", c.Issuer.String())
	}
	if n == 0 {
		return errors.New("no PEM block, and not a JWT")
	}
	return nil
}
