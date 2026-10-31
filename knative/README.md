# knative/

The examples of Field Guide 05, *the Knative Operator and Knative on kind*, from the Kubernetes
Notes zine series. The book installs the Knative Operator from its Helm chart (v1.23.1, then
v1.24.0) on No. 00's kind lab, runs Knative Serving, Eventing and Functions 1.24.0, and pins the
tag `knative-1.0`. Knative writes nothing to Git, so the book needs no fork: clone this repo
read-only at the tag. Functions are built on your machine and pushed only to a registry inside
the lab.

- `serving/`: the lab's KnativeServing, one whole file a state, each applied with
  `kubectl apply -f` in order: `01-kourier.yaml` (1.23 on Kourier, URLs under `localhost`),
  `02-secure.yaml` (Serving adds secure defaults to your containers), `03-v1.24.yaml` (the
  upgrade), `04-grace.yaml` (a shorter scale-to-zero grace period), `05-gateway.yaml` (Gateway
  API on No. 00's kgateway instead of Kourier), `06-mapping.yaml` (DomainMapping through the
  Route), `07-knobs.yaml` (high availability, a workload override), `08-metrics.yaml`
  (Prometheus metrics on).
- `eventing/`: the lab's KnativeEventing.
- `gateway/`: a second kgateway Gateway for cluster-local traffic.
- `workloads/`: namespace `demo` (Pod Security restricted), a curl Pod, and fortio for load.
- `services/`: the Knative Services, all No. 00's podinfo: `hello` (and `hello-bare`, before Pod
  Security), `busy`, `quick`, `one`, `steady`, `cpu`, `slow`, `inside`, and a DomainMapping.
- `events/`: namespace `events` with its Broker, event_display, a curl Pod and a failing
  subscriber; `triggers/`, `sources/` (PingSource, ApiServerSource, SinkBinding,
  ContainerSource) and `flows/` (Channel, Sequence, Parallel, JobSink).
- `functions/`: the registry on the lab.
- `breaks/`: things that fail on purpose: an ingress class nobody runs, a tag that does not
  exist, the wrong port, a failing probe, an image no node can pull, a filter that does not
  parse.
- `probe.sh`: asks a URL every 0.2 s and counts the answers, as the lab changes under it.
