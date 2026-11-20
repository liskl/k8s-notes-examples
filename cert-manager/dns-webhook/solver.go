package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// solverName is the last part of the API path, and the Issuer's webhook.solverName.
const solverName = "challtestsrv"

// client talks to the challenge test server's management API. A solver that hangs holds up
// cert-manager's Challenge, so every call has a deadline.
var client = &http.Client{Timeout: 10 * time.Second}

// solver sets and clears TXT records on pebble-challtestsrv. It keeps no state: each call
// carries the Issuer's config, so one webhook can serve Issuers that point at different servers.
type solver struct{}

var _ webhook.Solver = (*solver)(nil)

// config is the JSON under webhook.config in the Issuer's solver.
type config struct {
	// ManagementURL is the test server's management API, for example
	// http://challtestsrv.pebble.svc.cluster.local:8055
	ManagementURL string `json:"managementURL"`
}

func (s *solver) Name() string { return solverName }

// Initialize runs once at startup. A solver that reads Secrets would build its Kubernetes
// client from kubeConfig here. This one needs nothing from the cluster.
func (s *solver) Initialize(kubeConfig *rest.Config, stopCh <-chan struct{}) error { return nil }

// Present publishes the challenge: a TXT record named ch.ResolvedFQDN (for example
// "_acme-challenge.lab.example.", with its trailing dot) whose value is ch.Key.
func (s *solver) Present(ch *v1alpha1.ChallengeRequest) error {
	err := s.post(ch, "/set-txt", map[string]string{"host": ch.ResolvedFQDN, "value": ch.Key})
	return logged("present", ch, err)
}

// CleanUp removes the record once the challenge is over, valid or not.
//
// Caveat: /clear-txt has no "value" field. It removes every TXT value of the host. A wildcard
// and its apex (*.lab.example and lab.example) put two values on one name, so cleaning up one
// would break the other if both were live at once. It is safe only because cert-manager runs
// the Challenges of one DNS name one after the other. A real solver must delete only ch.Key.
func (s *solver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	err := s.post(ch, "/clear-txt", map[string]string{"host": ch.ResolvedFQDN})
	return logged("cleanup", ch, err)
}

// post sends one JSON body to the management API named in the challenge's config.
func (s *solver) post(ch *v1alpha1.ChallengeRequest, path string, body map[string]string) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding the request: %w", err)
	}
	target := strings.TrimRight(cfg.ManagementURL, "/") + path
	resp, err := client.Post(target, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("calling the challenge test server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("POST %s: the challenge test server answered %s: %s",
			target, resp.Status, strings.TrimSpace(string(text)))
	}
	return nil
}

// loadConfig decodes and checks the Issuer's webhook.config.
func loadConfig(raw *extapi.JSON) (config, error) {
	var cfg config
	if raw == nil || len(raw.Raw) == 0 {
		return cfg, errors.New("solver config: missing, set webhook.config.managementURL in the Issuer")
	}
	if err := json.Unmarshal(raw.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("solver config: %w", err)
	}
	u, err := url.Parse(cfg.ManagementURL)
	if err != nil {
		return cfg, fmt.Errorf("solver config: managementURL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return cfg, fmt.Errorf("solver config: managementURL %q must be an http or https URL", cfg.ManagementURL)
	}
	return cfg, nil
}

// logged writes one line per call and passes the error on. The key is what anyone can read
// from DNS a moment later, but a log does not need it: its length is enough to see it arrived.
func logged(action string, ch *v1alpha1.ChallengeRequest, err error) error {
	fields := []any{"fqdn", ch.ResolvedFQDN, "zone", ch.ResolvedZone, "keyLength", len(ch.Key)}
	if err != nil {
		klog.ErrorS(err, action+" failed", fields...)
		return err
	}
	klog.InfoS(action+" ok", fields...)
	return nil
}
