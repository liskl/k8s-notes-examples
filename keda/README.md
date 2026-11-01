# keda/

The examples of Field Guide 06, *KEDA on kind*, from the Kubernetes Notes zine series. The book runs KEDA
v2.21.0 (chart `keda` 2.21.0) and the KEDA HTTP Add-on v0.16.0 (chart `keda-add-ons-http` 0.16.0) on
No. 00's kind lab, with metrics-server and the event sources it scales on. KEDA writes nothing to Git, so
the book needs no fork: clone this repo read-only at the tag the book names.

- `demo-image/`: the book's demo image, `keda-demo`, one static Go binary: producers and slow workers for
  Redis lists and streams, RabbitMQ, Kafka and PostgreSQL, a one-shot worker for a ScaledJob, the server
  KEDA's metrics-api scaler reads (it can check a bound service account token's audience), a KEDA external
  scaler, a CPU and memory burner, a small HTTP load generator and a slow-starting web app. It never prints
  a password or a token. The workflow `.github/workflows/keda-demo-image.yaml` builds it for linux/amd64
  and linux/arm64 and publishes it to `ghcr.io/<owner>/k8s-notes-examples/keda-demo`; the book pins it by
  digest. `demo-image/externalscaler/` is KEDA's own gRPC contract for external scalers, copied unchanged
  from `github.com/kedacore/keda` v2.21.0 (`pkg/scalers/externalscaler`) under the Apache License 2.0,
  whose text is beside it.
