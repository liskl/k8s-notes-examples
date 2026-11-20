// lab-issuer is the reader's own issuer for cert-manager, built on issuer-lib: two kinds, LabIssuer and
// LabClusterIssuer, that sign from a CA in a Secret and hold every request to three house rules.
// It is a teaching issuer for a kind lab, not for production. README.md has the rest.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cert-manager/issuer-lib/api/v1alpha1"
	"github.com/cert-manager/issuer-lib/controllers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// version is set by the build: -ldflags "-X main.version=1.0.0".
var version = "dev"

const fieldOwner = "lab-issuer.issuer.lab.example"

func main() {
	zone := flag.String("zone", "lab.example", "house rule 1: sign only this DNS name and the names under it")
	maxLifetime := flag.Duration("max-lifetime", 24*time.Hour, "house rule 3: shorten any longer certificate to this lifetime")
	clusterNS := flag.String("cluster-resource-namespace", os.Getenv("POD_NAMESPACE"),
		"the namespace a LabClusterIssuer's Secret is read from (default: $POD_NAMESPACE)")
	setCA := flag.Bool("set-ca", false, "also hand the root to cert-manager, which writes it as ca.crt in each Certificate's Secret")
	maxRetry := flag.Duration("max-retry-duration", 2*time.Minute, "how long a request that keeps failing is retried before it fails for good")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	r := rules{zone: strings.ToLower(strings.Trim(*zone, ".")), maxLifetime: *maxLifetime}
	switch {
	case r.zone == "":
		fatal("-zone must name a DNS zone")
	case r.maxLifetime <= 0:
		fatal("-max-lifetime must be more than zero")
	case *clusterNS == "":
		fatal("-cluster-resource-namespace is empty and POD_NAMESPACE is not set")
	}

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("lab-issuer")

	scheme := runtime.NewScheme()
	addToScheme(scheme)
	if err := corev1.AddToScheme(scheme); err != nil {
		fatal(err.Error())
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: ":8080"},
		HealthProbeBindAddress: ":8081",
		LeaderElection:         true,
		LeaderElectionID:       fieldOwner,
	})
	if err != nil {
		fatal(err.Error())
	}
	s := labSigner{reader: mgr.GetAPIReader(), clusterNS: *clusterNS, rules: r}
	ctx := ctrl.SetupSignalHandler()
	if err := (&controllers.CombinedController{
		IssuerTypes:        []v1alpha1.Issuer{&LabIssuer{}},
		ClusterIssuerTypes: []v1alpha1.Issuer{&LabClusterIssuer{}},
		FieldOwner:         fieldOwner,
		MaxRetryDuration:   *maxRetry,
		Sign:               s.Sign,
		Check:              s.Check,
		EventRecorder:      mgr.GetEventRecorder("lab-issuer"),
		// Off by default, as in issuer-lib: without it a Certificate's Secret has no ca.crt, even
		// though Sign returns the root every time.
		SetCAOnCertificateRequest: *setCA,
		// cert-manager's CertificateRequests only. With the Kubernetes CSR controller on, the
		// ServiceAccount would also need rules for certificatesigningrequests and their signers.
		DisableKubernetesCSRController: true,
	}).SetupWithManager(ctx, mgr); err != nil {
		fatal(err.Error())
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		fatal(err.Error())
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		fatal(err.Error())
	}
	log.Info("starting", "version", version, "zone", r.zone, "maxLifetime", r.maxLifetime.String(),
		"clusterResourceNamespace", *clusterNS, "setCA", *setCA, "maxRetryDuration", maxRetry.String())
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "lab-issuer: "+msg)
	os.Exit(1)
}
