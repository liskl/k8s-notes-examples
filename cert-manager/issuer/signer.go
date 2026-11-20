package main

// Check and Sign: the two functions an issuer built on issuer-lib has to write. issuer-lib does the
// rest: it watches the issuers and the CertificateRequests, waits for a request to be approved, calls
// Sign, and writes the conditions and Events from the kind of error Sign returns.
//
//	a plain error            -> "will retry", with backoff, until -max-retry-duration has passed
//	signer.PermanentError    -> the request fails for good (cert-manager makes a new one later)
//	signer.IssuerError       -> the issuer is checked again, and the request waits for it

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/cert-manager/issuer-lib/api/v1alpha1"
	"github.com/cert-manager/issuer-lib/controllers/signer"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type labSigner struct {
	reader    client.Reader // reads Secrets straight from the API server, so RBAC needs "get" only
	clusterNS string        // where a LabClusterIssuer's Secret lives
	rules     rules
}

// authorityFor reads the Secret the issuer names and returns its CA. A Secret that is not there (yet)
// is a plain error, so the caller retries. A Secret that is there and wrong is a PermanentError.
func (s labSigner) authorityFor(ctx context.Context, iss v1alpha1.Issuer) (*authority, error) {
	var name, ns string
	switch i := iss.(type) {
	case *LabIssuer:
		name, ns = i.Spec.SecretName, i.Namespace
	case *LabClusterIssuer:
		name, ns = i.Spec.SecretName, s.clusterNS
	default:
		return nil, signer.PermanentError{Err: fmt.Errorf("not an issuer of this group: %T", iss)}
	}
	var sec corev1.Secret
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec); err != nil {
		return nil, err
	}
	a, err := loadAuthority(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey], sec.Data["ca.crt"])
	if err != nil {
		return nil, signer.PermanentError{Err: fmt.Errorf("secret %s/%s: %w", ns, name, err)}
	}
	return a, nil
}

// Check decides the issuer's Ready condition.
func (s labSigner) Check(ctx context.Context, iss v1alpha1.Issuer) error {
	_, err := s.authorityFor(ctx, iss)
	return err
}

// Sign signs one approved CertificateRequest, or says why not.
func (s labSigner) Sign(ctx context.Context, cr signer.CertificateRequestObject, iss v1alpha1.Issuer) (signer.PEMBundle, error) {
	log := ctrl.Log.WithName("sign").WithValues("request", cr.GetNamespace()+"/"+cr.GetName())

	a, err := s.authorityFor(ctx, iss)
	if errors.As(err, &signer.PermanentError{}) {
		// The Secret went wrong after Check passed. That is the issuer's fault, not the request's:
		// hand it back to the issuer's own controller, and let the request wait.
		log.Info("not signed: the issuer's CA is wrong", "reason", err.Error())
		return signer.PEMBundle{}, signer.IssuerError{Err: err}
	}
	if err != nil {
		log.Info("not signed: will retry", "reason", err.Error())
		return signer.PEMBundle{}, err
	}
	bundle, leaf, err := s.issue(a, cr)
	if err != nil {
		log.Info("refused", "reason", err.Error())
		return signer.PEMBundle{}, err
	}
	log.Info("signed", "serial", leaf.SerialNumber.Text(16), "dnsNames", leaf.DNSNames,
		"notAfter", leaf.NotAfter.UTC().Format(time.RFC3339), "ca", a.cert.Subject.String())
	return bundle, nil
}

// issue is Sign without the cluster: the house rules, then the signature.
func (s labSigner) issue(a *authority, cr signer.CertificateRequestObject) (signer.PEMBundle, *x509.Certificate, error) {
	details, err := cr.GetCertificateDetails()
	if err != nil {
		return signer.PEMBundle{}, nil, signer.PermanentError{Err: err}
	}
	tmpl, err := s.rules.template(details)
	if err != nil {
		return signer.PEMBundle{}, nil, signer.PermanentError{Err: err}
	}
	return a.sign(tmpl)
}
