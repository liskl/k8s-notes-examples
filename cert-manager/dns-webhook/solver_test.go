package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/letsencrypt/challtestsrv"
	"github.com/miekg/dns"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// These tests use no mocks of DNS. They start the real challenge test server library
// (github.com/letsencrypt/challtestsrv, the version Pebble 2.10.3 builds on) with its DNS server
// on a loopback port, and read the solver's records back with real DNS queries.
//
// The library has no HTTP management API of its own: /set-txt and /clear-txt live in Pebble's
// cmd/pebble-challtestsrv, a package main that cannot be imported. management() below restates
// those two handlers (dns.go and http.go at Pebble v2.10.3: the same paths, JSON fields and
// status codes) on top of the same library calls. The real binary is used in the lab itself.

// lab is one running challenge test server.
type lab struct {
	dnsAddr       string // host:port of its DNS server
	managementURL string
}

func startLab(t *testing.T) lab {
	t.Helper()
	// Ask the kernel for a free port, then hand it to the library, which wants an address.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()

	srv, err := challtestsrv.New(challtestsrv.Config{
		DNSAddrs: []string{addr},
		Log:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Run()
	t.Cleanup(srv.Shutdown)

	mgmt := httptest.NewServer(management(srv))
	t.Cleanup(mgmt.Close)

	l := lab{dnsAddr: addr, managementURL: mgmt.URL}
	// Wait until the DNS server answers.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := l.query("ready.lab.example."); err == nil {
			return l
		} else if time.Now().After(deadline) {
			t.Fatalf("the DNS server on %s never answered: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// management restates pebble-challtestsrv's /set-txt and /clear-txt handlers.
func management(srv *challtestsrv.ChallSrv) http.Handler {
	parse := func(w http.ResponseWriter, r *http.Request, into any) bool {
		body, err := io.ReadAll(r.Body)
		if err == nil && len(body) == 0 {
			http.Error(w, "expected JSON POST body, was empty", http.StatusBadRequest)
			return false
		}
		if err == nil {
			err = json.Unmarshal(body, into)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/set-txt", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Host, Value string }
		if !parse(w, r, &req) {
			return
		}
		if req.Host == "" || req.Value == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		srv.AddDNSTXTRecord(req.Host, req.Value)
	})
	mux.HandleFunc("/clear-txt", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Host string }
		if !parse(w, r, &req) {
			return
		}
		if req.Host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		srv.DeleteDNSTXTRecord(req.Host)
	})
	return mux
}

// query asks the lab's DNS server for the TXT values of a name.
func (l lab) query(fqdn string) ([]string, error) {
	m := new(dns.Msg)
	m.SetQuestion(fqdn, dns.TypeTXT)
	in, _, err := new(dns.Client).Exchange(m, l.dnsAddr)
	if err != nil {
		return nil, err
	}
	var values []string
	for _, rr := range in.Answer {
		if txt, ok := rr.(*dns.TXT); ok {
			values = append(values, strings.Join(txt.Txt, ""))
		}
	}
	slices.Sort(values)
	return values, nil
}

func (l lab) txt(t *testing.T, fqdn string) []string {
	t.Helper()
	values, err := l.query(fqdn)
	if err != nil {
		t.Fatalf("TXT query for %s: %v", fqdn, err)
	}
	return values
}

// challenge builds what cert-manager sends for one Challenge.
func challenge(managementURL, fqdn, key string) *v1alpha1.ChallengeRequest {
	cfg, _ := json.Marshal(config{ManagementURL: managementURL})
	return &v1alpha1.ChallengeRequest{
		Action:       v1alpha1.ChallengeActionPresent,
		ResolvedFQDN: fqdn,
		ResolvedZone: "lab.example.",
		Key:          key,
		Config:       &extapi.JSON{Raw: cfg},
	}
}

func TestPresentThenCleanUp(t *testing.T) {
	l := startLab(t)
	s := &solver{}
	const fqdn = "_acme-challenge.lab.example."
	ch := challenge(l.managementURL, fqdn, "key-one")

	if got := l.txt(t, fqdn); len(got) != 0 {
		t.Fatalf("before Present: TXT = %q, want none", got)
	}
	if err := s.Present(ch); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if got := l.txt(t, fqdn); !slices.Equal(got, []string{"key-one"}) {
		t.Fatalf("after Present: TXT = %q, want [key-one]", got)
	}
	if err := s.CleanUp(ch); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := l.txt(t, fqdn); len(got) != 0 {
		t.Fatalf("after CleanUp: TXT = %q, want none", got)
	}
	// cert-manager may call CleanUp again: clearing a name with no record is not an error.
	if err := s.CleanUp(ch); err != nil {
		t.Fatalf("second CleanUp: %v", err)
	}
}

// A trailing slash on managementURL must not turn the path into //set-txt.
func TestManagementURLWithTrailingSlash(t *testing.T) {
	l := startLab(t)
	const fqdn = "_acme-challenge.slash.lab.example."
	if err := (&solver{}).Present(challenge(l.managementURL+"/", fqdn, "key-slash")); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if got := l.txt(t, fqdn); !slices.Equal(got, []string{"key-slash"}) {
		t.Fatalf("TXT = %q, want [key-slash]", got)
	}
}

// The caveat in solver.go, pinned down: a wildcard and its apex share one record name, and
// CleanUp for either removes both values. This is the test server's behaviour, not a bug to fix
// here. If this test starts failing, /clear-txt has learned to take a value and the caveat can go.
func TestCleanUpClearsEveryValueOfTheName(t *testing.T) {
	l := startLab(t)
	s := &solver{}
	const fqdn = "_acme-challenge.lab.example."
	wildcard := challenge(l.managementURL, fqdn, "key-wildcard")
	apex := challenge(l.managementURL, fqdn, "key-apex")

	for _, ch := range []*v1alpha1.ChallengeRequest{wildcard, apex} {
		if err := s.Present(ch); err != nil {
			t.Fatalf("Present: %v", err)
		}
	}
	if got := l.txt(t, fqdn); !slices.Equal(got, []string{"key-apex", "key-wildcard"}) {
		t.Fatalf("after two Presents: TXT = %q, want both keys", got)
	}
	if err := s.CleanUp(wildcard); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := l.txt(t, fqdn); len(got) != 0 {
		t.Fatalf("after one CleanUp: TXT = %q, want none (the test server clears the whole name)", got)
	}
}

func TestErrorsNameTheCause(t *testing.T) {
	l := startLab(t)
	s := &solver{}

	// An empty key makes the real handler answer 400.
	err := s.Present(challenge(l.managementURL, "_acme-challenge.lab.example.", ""))
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request") {
		t.Errorf("empty key: err = %v, want one that names 400 Bad Request", err)
	}

	// A path the server does not have: 404.
	err = s.Present(challenge(l.managementURL+"/nothing-here", "_acme-challenge.lab.example.", "k"))
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("wrong path: err = %v, want one that names 404 Not Found", err)
	}

	// Nothing listens here: the dial error must come through.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	err = s.Present(challenge(deadURL, "_acme-challenge.lab.example.", "k"))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("dead server: err = %v, want one that says connection refused", err)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     *extapi.JSON
		want    string // the managementURL, when valid
		wantErr string // a piece of the error, when not
	}{
		{name: "http", raw: js(`{"managementURL":"http://challtestsrv.pebble.svc.cluster.local:8055"}`),
			want: "http://challtestsrv.pebble.svc.cluster.local:8055"},
		{name: "https", raw: js(`{"managementURL":"https://challtestsrv.example:8055/"}`),
			want: "https://challtestsrv.example:8055/"},
		{name: "no config", raw: nil, wantErr: "missing"},
		{name: "empty config", raw: js(``), wantErr: "missing"},
		{name: "empty object", raw: js(`{}`), wantErr: "must be an http or https URL"},
		{name: "not JSON", raw: js(`managementURL: http://x`), wantErr: "solver config"},
		{name: "wrong type", raw: js(`{"managementURL":8055}`), wantErr: "solver config"},
		{name: "no scheme", raw: js(`{"managementURL":"challtestsrv.pebble:8055"}`), wantErr: "must be an http or https URL"},
		{name: "other scheme", raw: js(`{"managementURL":"ftp://challtestsrv.pebble:8055"}`), wantErr: "must be an http or https URL"},
		{name: "no host", raw: js(`{"managementURL":"http://"}`), wantErr: "must be an http or https URL"},
		{name: "unparsable", raw: js(`{"managementURL":"http://bad host/"}`), wantErr: "managementURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if cfg.ManagementURL != tt.want {
				t.Fatalf("managementURL = %q, want %q", cfg.ManagementURL, tt.want)
			}
		})
	}
}

// A bad config must stop Present before any request is made.
func TestPresentRejectsBadConfig(t *testing.T) {
	ch := &v1alpha1.ChallengeRequest{ResolvedFQDN: "_acme-challenge.lab.example.", Key: "k"}
	if err := (&solver{}).Present(ch); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want the missing config error", err)
	}
}

func TestName(t *testing.T) {
	if got := (&solver{}).Name(); got != "challtestsrv" {
		t.Fatalf("Name() = %q, want challtestsrv", got)
	}
}

func js(s string) *extapi.JSON { return &extapi.JSON{Raw: []byte(s)} }
