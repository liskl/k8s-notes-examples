# cert-manager/

The examples of Field Guide 09, *cert-manager on kind*, from the Kubernetes Notes zine series. The book runs
cert-manager v1.21.3 (its chart, with cmctl v2.6.2) on No. 00's kind lab, with trust-manager v0.26.0,
cert-manager-csi-driver v0.16.1 and cert-manager-approver-policy v0.29.0. Its ACME server is Pebble v2.10.3,
inside the lab: the book never talks to Let's Encrypt or to any certificate authority outside the cluster.
Chapter 13's Flux (2.10.0, through the Flux Operator v0.62.1) only reads this repo over anonymous HTTPS, so the
book needs no fork: clone it read-only at the tag `cert-manager-1.0`.

Every chart is held to a checksum and every image is named by digest. **No file here holds a private key, a
token or a certificate.** The lab makes its own certificate authority when the book is followed, and the book's
printouts show a Secret's keys and a certificate's fields, never a key.

The lab is No. 00's with one line more in `kind.yaml`: host port 8443 to nodePort 30443 on `lab-worker`, the
HTTPS door chapter 5 opens. The names the book proves are under `lab.example`, a zone that exists only in the lab.

- `install/`: the sha256 sums of the four charts, cmctl, the `flux` CLI and the Flux Operator's install manifest.
- `values/`: the charts' values. `cert-manager.yaml` is the book's install; `cert-manager-dns01.yaml` (chapter 7)
  and `cert-manager-strict.yaml` (chapter 10) are added to it with a second `-f`.
- `first/`: chapter 2: a SelfSigned Issuer, the first Certificate, and three small breaks.
- `pki/`: chapter 3: a root, an intermediate, a ClusterIssuer on each, a leaf, and two CA issuers that cannot work.
- `life/`: chapter 4: a one-hour certificate that renews after five minutes, one that keeps its key, two that
  name one Secret, and an issuer whose key does not match its certificate.
- `door/`: chapter 5: the Certificate for `www.lab.example`, No. 00's GatewayParameters and Gateway with port
  443 and an HTTPS listener, the route, and the Gateway annotated so cert-manager writes the Certificates itself.
- `acme/`: chapter 6: Pebble, its serving certificate from the lab's CA, the ACME ClusterIssuer with an HTTP-01
  solver on the Gateway, the one CoreDNS line that makes lab names resolve, and an issuer whose solver is lost.
- `dns/`: chapter 7: a BIND that is the authority for `_acme-challenge.lab.example`, the CoreDNS block that
  sends the zone to it, the `rfc2136` issuer without and with a TSIG key, and two wildcards.
- `mtls/`, `trust/`: chapter 8: a server and a client certificate, the demo image as both ends, a certificate
  with the wrong usage; then trust-manager's Bundle and the same apps trusting it.
- `csi/`: chapter 9: a Pod whose certificate comes from a csi volume, and two that cannot start.
- `policy/`: chapter 10: a CertificateRequestPolicy a team with its RBAC, the platform's own, and the requests
  that pass and fail them.
- `issuer/`, `own/`: chapter 11: the book's issuer in Go on issuer-lib (its own README says what it does and
  how to test and build it), and the CA, the issuers and the requests the chapter gives it.
- `dns-webhook/`, `solver/`: chapter 12: the book's DNS-01 webhook solver in Go for Pebble's challenge test
  server (its own README), the test server, the CoreDNS block for it, its issuer and two wildcards.
- `gitops/`: chapter 13: what Flux reads on the second cluster.
- `demo-image/`: the demo image, `cert-manager-demo`: a small TLS server and client and a certificate reader
  that never prints a key (its own README).

The three images are built once by this repository's workflows `cert-manager-issuer`, `cert-manager-dns-webhook`
and `cert-manager-demo-image`, and the manifests here pin them by digest:

| Image | Tag | Digest |
|---|---|---|
| `ghcr.io/liskl/k8s-notes-examples/lab-issuer` | 1.0.1 | `sha256:567ab191a5a07fe6eeec5ed8b5e520b7c67eea098b7ad2b8b96b49995cc93b5a` |
| `ghcr.io/liskl/k8s-notes-examples/lab-dns-webhook` | 1.0.1 | `sha256:5afae39f507d3a4ea4c4039c989f2dc3139a642db4a4f4b328623954830e565f` |
| `ghcr.io/liskl/k8s-notes-examples/cert-manager-demo` | 1.0.0 | `sha256:05f8e598f9b7b1f03e58bf9c43243cb1ee0870ed1113ce18b827c7e93fe86346` |

The issuer and the solver are teaching programs for a lab. Neither is meant for production.
