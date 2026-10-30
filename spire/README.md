# spire/

The examples of Field Guide 04, *SPIFFE and SPIRE on kind*, from the Kubernetes Notes zine series. The book
runs SPIRE v1.15.5 from the hardened Helm charts (`spire-crds` 0.6.1, then `spire` 0.30.4) on No. 00's kind
lab, and pins the tag `spire-1.0`. SPIRE writes nothing to Git, so the book needs no fork: clone this repo
read-only at the tag.

- `values/`: the charts' values, one file a change, each applied with `helm upgrade --reuse-values -f`:
  `base.yaml` (trust domain `lab.k8s-notes.test`, the production layout), `all-nodes.yaml` (control-plane
  tolerations), `kubelet-hostcert.yaml` (the agent checks the kubelet's certificate), `federation.yaml` (serve
  the bundle endpoint), `partner.yaml` (a second SPIRE, release `spire-b`, trust domain
  `partner.k8s-notes.test`), `rebootstrap.yaml` (a shorter rebootstrap delay for the lab).
- `workloads/`: namespace `demo` (Pod Security restricted) and the book's workloads, all `spiffe-demo`:
  `whoami`, `fetch` (SPIRE's own `spire-agent api fetch x509`), `byhand`, `backend` and `client`.
- `ids/`: ClusterSPIFFEIDs and ClusterStaticEntries: `by-app.yaml`, `no-class.yaml`, `static.yaml`,
  `by-app-federated.yaml`.
- `envoy/`: two Envoy configurations (SDS from the agent) and their workloads.
- `helper/`: spiffe-helper's configuration and a workload that reads its certificate from files.
- `federation/`: the two ClusterFederatedTrustDomains and the partner's namespace, identity and server.
- `fast/`: namespace `fast`, where SVIDs live 2 minutes and JWT-SVIDs 1 minute.
- `podcert/`: a Pod asking Kubernetes (not SPIRE) for a Pod certificate, from a signer nobody runs.
- `breaks/`: things that fail on purpose: a CSI driver nobody registered, a kubelet check that cannot pass on
  kind, three agents that node attestation refuses.
- `demo-image/`: the book's demo workload, `spiffe-demo`, one static go-spiffe binary that asks the SPIRE
  agent for its identity and prints it (never a private key or a whole token). The workflow
  `.github/workflows/spire-demo-image.yaml` builds it for linux/amd64 and linux/arm64 and publishes it to
  `ghcr.io/<owner>/k8s-notes-examples/spiffe-demo`; the book pins it by digest.
