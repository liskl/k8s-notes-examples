# lab-issuer

The reader's own issuer for cert-manager, from Field Guide 09. It adds two kinds to the cluster,
`LabIssuer` (namespaced) and `LabClusterIssuer` (cluster-scoped), in the group `issuer.lab.example`.
Each names a Secret that holds a CA, signs the CertificateRequests that name it, and holds every request
to three house rules.

It is built on [issuer-lib](https://github.com/cert-manager/issuer-lib) v0.12.1, which does the
controller work. The code here is what an issuer has to bring: the two kinds (`api.go`), reading a CA,
the rules and the signature (`ca.go`), and the two functions issuer-lib calls, `Check` and `Sign`
(`signer.go`).

**This is a teaching issuer for a kind lab. Do not run it in production.** It keeps its CA key in a
Secret, has no audit trail, no revocation and no rate limit, and has had no security review.

## The house rules

A request that breaks rule 1 or 2 fails for good, with a message that starts `house rule:` on the
CertificateRequest. Rule 3 never refuses.

1. **Names in the zone only.** Every DNS name must be `lab.example` or end in `.lab.example`. A request
   with no DNS name is refused, and so is one with an IP address, a URI or an email address.
2. **No CAs.** A request with `isCA: true` is refused.
3. **A day at most.** A request for a longer lifetime is signed for 24 hours.

## An issuer

```yaml
apiVersion: issuer.lab.example/v1alpha1
kind: LabClusterIssuer
metadata:
  name: lab
spec:
  secretName: lab-issuer-ca   # tls.crt, tls.key and, if it has one, ca.crt
```

A LabIssuer's Secret is read in the LabIssuer's own namespace. A LabClusterIssuer's Secret is read in
the namespace `-cluster-resource-namespace` names. A Certificate asks for it with
`issuerRef: {group: issuer.lab.example, kind: LabClusterIssuer, name: lab}`.

The issuer's Ready condition says what it found in the Secret:

- No such Secret: not ready, and it looks again (with backoff; it does not watch Secrets).
- The certificate is not a CA, the key is not the certificate's key, or a value is not PEM: failed.
  That is final until the issuer object changes or the issuer's Pod restarts.

The certificate it returns is the leaf and then the CA's own `tls.crt`. It returns the root too (the
Secret's `ca.crt`, or else the CA certificate), but cert-manager writes that as `ca.crt` only when the
issuer runs with `-set-ca`.

## Flags

| Flag | Default | What it sets |
|---|---|---|
| `-zone` | `lab.example` | Rule 1: the DNS zone it signs names in |
| `-max-lifetime` | `24h` | Rule 3: the longest lifetime it signs |
| `-cluster-resource-namespace` | `$POD_NAMESPACE` | Where a LabClusterIssuer's Secret is read from |
| `-set-ca` | `false` | Hand the root to cert-manager, so each Certificate's Secret gets a `ca.crt` |
| `-max-retry-duration` | `2m` | How long a request that keeps failing is retried before it fails for good |
| `-kubeconfig` | in the cluster | A kubeconfig, to run it outside a cluster |
| `-version` | | Print the version and exit |

It serves metrics on `:8080` and `/healthz` and `/readyz` on `:8081`, and holds a Lease so only one
replica works at a time.

## Test, build, load

The tests need no cluster. They make their CAs and keys in memory and write none of them to a file.

```bash
go vet ./... && go test ./...

docker build -t lab-issuer:dev .          # runs the same vet and tests first
kind load docker-image lab-issuer:dev --name <cluster>
```

Without Go on the machine, run the tests in the image the Dockerfile builds with:

```bash
docker run --rm -v "$PWD":/src -w /src -e GOTOOLCHAIN=local \
  docker.io/library/golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d \
  sh -c 'go vet ./... && go test ./...'
```

## Deploy

```bash
kubectl apply -f deploy/crds.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deployment-local.yaml   # the image you loaded: lab-issuer:dev, never pulled
# or
kubectl apply -f deploy/deployment.yaml         # the published image
kubectl apply -f deploy/approve-rbac.yaml
```

`deploy/approve-rbac.yaml` lets cert-manager's own approver approve requests for the two kinds. Without
it (or the same two names in the chart's `approveSignerNames`) a request for a LabIssuer is never
approved, so it is never signed.

## Dependencies

`go.mod` requires issuer-lib v0.12.1, which brings controller-runtime v0.24.1, the Kubernetes libraries
v0.37.0 and cert-manager's library v1.21.1. `golang.org/x/net` and `golang.org/x/crypto` are raised
above what those ask for (to v0.62.0 and v0.59.0), so that `govulncheck ./...` finds nothing.

The image is published by the workflow `cert-manager-issuer` in this repository.
