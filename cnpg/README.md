# cnpg/

The examples of Field Guide 07, *CloudNativePG on kind*, from the Kubernetes Notes zine series. The book runs
the CloudNativePG operator 1.30.1 (chart `cloudnative-pg` 0.29.1) and the Barman Cloud Plugin v0.15.1 (chart
`plugin-barman-cloud` 0.8.1) on No. 00's kind lab, with cert-manager v1.21.3, RustFS 1.0.4 as the object
store, and csi-driver-host-path v1.19.0 with the external-snapshotter v8.7.0 for volume snapshots, and pins
the tag `cnpg-1.0`. CloudNativePG writes nothing to Git, so the book needs no fork: clone this repo read-only
at the tag.

- `install/`: the sha256 sums of the three charts and of the `kubectl cnpg` 1.30.1 release tarball.
- `values/`: the three charts' values. `cnpg.yaml` pins the operator's image by digest and puts it on
  `lab-worker` (the node the book never stops); `plugin.yaml` pins the plugin and its sidecar and puts it
  there too; `cert-manager.yaml` pins its five images and takes its CRDs away on `helm uninstall`. Every
  upgrade passes the same file again, since Helm starts each upgrade from the chart's defaults.
- `platform/`: what the book installs beside the operator: RustFS (`rustfs.yaml`, lab-only keys) and the Job
  that makes its bucket; the snapshot CRDs, the snapshot controller and the hostpath driver (`snapshot-crds/`,
  `snapshot-controller/`, `csi-hostpath/`: upstream's files at their tags, each image by digest, the driver's
  StatefulSet pinned to `lab-worker3`; Apache License 2.0, its text in `platform/LICENSE-APACHE-2.0`); and a
  plain Prometheus (`prometheus.yaml`) that finds what to scrape by listing Pods.
- `clusters/`: the namespace `apps` (Pod Security restricted) and the Cluster `app-db` a file a state, from
  the first three instances (`app-db.yaml`) through the pinned image, parameters, synchronous replication,
  roles, a pg_hba rule, an image-volume extension, WAL archiving, a server certificate of the book's own, the
  replica topology and a custom query; and `snap-db` on the hostpath class (data, WAL and a tablespace).
- `workloads/`: the book's demo, all `cnpg-demo` by digest: `demo.yaml` (the web app, its Service and an
  HTTPRoute on No. 00's Gateway), the writer (60 s, 150 s, through the pooler, on the other site) and the
  reader (as the app user, with a client certificate, against the book's own CA).
- `roles/`, `databases/`: a password Secret for a managed role, a DatabaseRole with a client certificate, and
  a Database with a schema and an extension.
- `poolers/`: PgBouncer in front of the primary (and paused), and in front of the replicas.
- `backups/`: the RustFS keys for `apps`, the ObjectStore, a Backup, a ScheduledBackup, and `snap-db`'s online
  and cold snapshot backups.
- `recovery/`: a point-in-time recovery of `app-db` from the bucket, a copy of `snap-db` from a snapshot, and
  a point-in-time recovery from a snapshot plus the WAL (the pages fill in the time).
- `pki/`: a CA of the book's own, made by cert-manager, and `app-db`'s server certificate signed by it.
- `replica/`: the second site, `apps-dr` (its ObjectStore and keys), and `app-db-dr`, a replica cluster of
  `app-db` in a distributed topology, trusting the book's CA for `app-db`'s certificate.
- `import/`: a new Cluster made by a logical import of `app-db`'s database.
- `upgrades/`: an image catalog of the book's own (17 and 18, by digest), a newer 17 build in it, and `up-db`,
  upgraded in place from 17 to 18.
- `monitoring/`: a custom query for the instances' exporter.
- `breaks/`: things that fail on purpose: a fixed parameter, four instances that each need a node of their
  own, a resize on local-path, a recovery that archives under its source's name, a replica cluster with
  PostgreSQL's defaults (a lower `max_connections` than its primary's), a StorageClass that does not exist, a
  bucket key that is wrong, a role's Secret with its password under the wrong key, and a NetworkPolicy that
  lets nothing in to the instances.
- `demo-image/`: the book's demo image, `cnpg-demo`, one static Go binary: a web app that says which instance
  answered (and whether it is the primary, and its timeline), a writer that counts the writes Postgres
  acknowledged through any outage and checks they are all still there, and a reader that follows a writer on
  the replicas. It takes its connection settings only from libpq's `PG*` variables and never prints a
  password. The workflow `.github/workflows/cnpg-demo-image.yaml` builds it for linux/amd64 and linux/arm64
  and publishes it to `ghcr.io/<owner>/k8s-notes-examples/cnpg-demo`; the book pins it by digest.
