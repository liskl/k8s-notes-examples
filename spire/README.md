# spire/

The examples of Field Guide 04, *SPIFFE and SPIRE on kind*, from the Kubernetes Notes zine series. The book
runs SPIRE v1.15.5 from the hardened Helm charts (`spire-crds` 0.6.1, then `spire` 0.30.4) on No. 00's kind
lab, and pins the tag `spire-1.0`.

- `demo-image/`: the book's demo workload, `spiffe-demo`, one static go-spiffe binary that asks the SPIRE
  agent for its identity and prints it (never a private key or a whole token). The workflow
  `.github/workflows/spire-demo-image.yaml` builds it for linux/amd64 and linux/arm64 and publishes it to
  `ghcr.io/<owner>/k8s-notes-examples/spiffe-demo`; the book pins it by digest.

SPIRE writes nothing to Git, so the book needs no fork: clone this repo read-only at the tag.
