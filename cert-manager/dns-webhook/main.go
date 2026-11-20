// lab-dns-webhook is a cert-manager DNS-01 webhook solver for Pebble's challenge test server
// (pebble-challtestsrv). It is a teaching solver for a lab, not for production.
//
// cert-manager does not call a webhook solver directly. The solver is served as an aggregated API
// (an APIService for the group below), cert-manager's controller POSTs a ChallengePayload to
// /apis/<group>/v1alpha1/<solver name> through the Kubernetes API server, and the library used
// here turns that into a call to Present or CleanUp. All the solver's own logic is in solver.go.
package main

import (
	"os"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	"k8s.io/klog/v2"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

func main() {
	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		groupName = "acme.lab.example"
	}
	klog.InfoS("lab-dns-webhook starting", "version", version, "group", groupName, "solver", solverName)
	cmd.RunWebhookServer(groupName, &solver{})
}
