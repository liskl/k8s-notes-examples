# keda/

The examples of Field Guide 06, *KEDA on kind*, from the Kubernetes Notes zine series. The book runs KEDA
v2.21.0 (chart `keda` 2.21.0) and the KEDA HTTP Add-on v0.16.0 (chart `keda-add-ons-http` 0.16.0) on
No. 00's kind lab, with metrics-server and the event sources it scales on, and pins the tag `keda-1.0`.
KEDA writes nothing to Git, so the book needs no fork: clone this repo read-only at the tag.

- `install/`: the sha256 sums of the two charts and of metrics-server v0.9.1's `components.yaml`, and the JSON
  patch that adds `--kubelet-insecure-tls` for kind's kubelets.
- `values/`: the keda chart's values. `keda.yaml` pins the three images by digest and turns on KEDA's own
  Prometheus metrics; `keda-audience.yaml` is KEDA 2.21's token audience mapping for `demo/metrics-reader`;
  `keda-legacy.yaml` is the insecure legacy mode, only to show what the CVE-2026-77524 fix stops. Every
  upgrade passes `keda.yaml` again, since Helm starts each upgrade from the chart's defaults.
- `columns/`: `kubectl get -o custom-columns-file=` layouts for ScaledObjects, ScaledJobs and Events, so their
  tables fit a page.
- `sources/`: one file a source, each in its own namespace under Pod Security restricted: Redis 8.10.3,
  RabbitMQ 4.4.0 (with the management plugin), Kafka 4.3.2 (one KRaft node), PostgreSQL 18.6 and
  Prometheus v3.15.1. Their passwords are lab-only strings.
- `workloads/`: namespace `demo` (restricted, with the Secret `lab-passwords` and the `toolbox`), and the
  apps the book scales, all `keda-demo` by digest: workers for each source, small web apps, the metrics-api
  receiver `values` (and two variants on other audiences), the external `scaler`, and `burner` and `holder`
  for cpu and memory.
- `scalers/`: the ScaledObjects and TriggerAuthentications each chapter applies, a file a state (for example
  `list-worker.yaml` and its `-activation`, `-idle`, `-behavior`, `-min1` and `-fallback` variants).
- `jobs/`: the ScaledJobs: `tickets`, one a scaling strategy (`batch-default`, `-accurate`, `-eager`), the
  two rollout strategies, and `stuck`, whose Jobs never finish.
- `http/`: the HTTP Add-on's side: namespace `web` and `site`, its InterceptorRoute, ScaledObject, HTTPRoute
  on No. 00's Gateway and ReferenceGrant, the slow-starting `slow` with and without a longer route timeout,
  and a host with no InterceptorRoute.
- `breaks/`: things that fail on purpose: each admission webhook rule broken alone, a plain HPA made after
  KEDA's, MessageRate over AMQP, a database host that does not resolve, an external scaler that does not
  exist, a wrong Secret key, a missing TriggerAuthentication, a target typo, a target with no `/scale`, and
  a failure message too long to be an Event.
- `demo-image/`: the book's demo image, `keda-demo`, one static Go binary: producers and slow workers for
  Redis lists and streams, RabbitMQ, Kafka and PostgreSQL, a one-shot worker for a ScaledJob, the server
  KEDA's metrics-api scaler reads (it can check a bound service account token's audience), a KEDA external
  scaler, a CPU and memory burner, a small HTTP load generator and a slow-starting web app. It never prints
  a password or a token. The workflow `.github/workflows/keda-demo-image.yaml` builds it for linux/amd64
  and linux/arm64 and publishes it to `ghcr.io/<owner>/k8s-notes-examples/keda-demo`; the book pins it by
  digest. `demo-image/externalscaler/` is KEDA's own gRPC contract for external scalers, copied unchanged
  from `github.com/kedacore/keda` v2.21.0 (`pkg/scalers/externalscaler`) under the Apache License 2.0,
  whose text is beside it.
