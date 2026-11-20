# lab-dns-webhook

A cert-manager DNS-01 webhook solver for Pebble's challenge test server (`pebble-challtestsrv`).

It is a teaching solver for a lab: small enough to read in one sitting, and written for a test
server whose records anyone in the cluster can change. Do not point it at anything you care about.

- `solver.go`: all of the solver. `Present` POSTs `/set-txt`, `CleanUp` POSTs `/clear-txt`.
- `main.go`: wiring only. It hands the solver to cert-manager's webhook server library.
- `solver_test.go`: the tests (below).
- `deploy/`: plain manifests, applied in file order.
- `Dockerfile`: the image `lab-dns-webhook`, a static binary on distroless.

Built against cert-manager v1.21.3 (`github.com/cert-manager/cert-manager v1.21.3`) and tested on
Kubernetes v1.37 (kind) with Pebble and `pebble-challtestsrv` 2.10.3.

## How it fits

cert-manager does not call a webhook solver directly. The solver runs as an aggregated API server:

```
cert-manager controller
   |  POST /apis/acme.lab.example/v1alpha1/challtestsrv   (a ChallengePayload)
   v
Kubernetes API server  -- APIService v1alpha1.acme.lab.example -->  Service lab-dns-webhook:443
                                                                        |
                                                                        v
                                                    this program: Present / CleanUp
                                                                        |  POST /set-txt, /clear-txt
                                                                        v
                                                    pebble-challtestsrv (management API, port 8055)
```

Three names must agree, or the request never arrives:

| Name | Where it is set |
|---|---|
| the group, `acme.lab.example` | `GROUP_NAME` on the Deployment, the APIService, the domain-solver ClusterRole, and `groupName` in the Issuer |
| the solver, `challtestsrv` | `solverName` in `solver.go`, the resource in the domain-solver ClusterRole, and `solverName` in the Issuer |
| the caller, ServiceAccount `cert-manager` in namespace `cert-manager` | the domain-solver ClusterRoleBinding |

### The Issuer

```yaml
solvers:
  - dns01:
      webhook:
        groupName: acme.lab.example
        solverName: challtestsrv
        config:
          managementURL: http://challtestsrv.pebble.svc.cluster.local:8055
```

`config.managementURL` is required and must be an `http` or `https` URL. It travels with every
request, so one webhook can serve Issuers that point at different test servers.

### What the lab needs around it

The webhook only writes the record. Two more things have to be true before cert-manager will even
call it, and neither is this program's job.

1. **Something must answer the zone's SOA.** Before it presents a challenge, cert-manager looks up
   the SOA of `_acme-challenge.<name>` to find the zone. `pebble-challtestsrv` answers SOA queries
   with `NOTIMP`. A second server block in CoreDNS answers the SOA itself and forwards everything
   else under the zone to the test server (here its Service has the fixed ClusterIP `10.96.0.54`,
   with port 53 on both UDP and TCP):

   ```
   lab.example:53 {
       errors
       template IN SOA lab.example {
           answer "lab.example. 30 IN SOA ns.lab.example. root.lab.example. 1 7200 3600 1209600 30"
       }
       forward . 10.96.0.54:53
   }
   ```

2. **cert-manager must ask CoreDNS, and only CoreDNS.** Two Helm values on the cert-manager chart
   (`10.96.0.10` is `kube-dns` on kind):

   ```
   --set dns01RecursiveNameservers=10.96.0.10:53
   --set dns01RecursiveNameserversOnly=true
   ```

Pebble needs no `-dnsserver` flag: its own resolver is CoreDNS too, so it follows the same block.

## The caveat: `/clear-txt` clears the whole name

`/set-txt` adds a value to a name. `/clear-txt` takes only the name and removes every TXT value
it has. A wildcard and its apex (`*.lab.example` and `lab.example`) are proved with two values on
the same name, `_acme-challenge.lab.example.`, so cleaning up one would remove the other if both
were live at once.

It works in practice because cert-manager runs the Challenges of one DNS name one after the other:
present, wait, verify, clean up, and only then the next. `TestCleanUpClearsEveryValueOfTheName`
pins the test server's behaviour down. A solver for a real DNS provider must delete only the value
it was given (`ch.Key`).

## Test

```sh
go vet ./... && go test ./...
```

The tests use no mock DNS. They start the real challenge test server library
(`github.com/letsencrypt/challtestsrv` v1.4.2, the version Pebble 2.10.3 builds on) with its DNS
server on a loopback port, call `Present` and `CleanUp`, and read the records back with real DNS
TXT queries. One honest limit: the library has no HTTP management API of its own. `/set-txt` and
`/clear-txt` live in Pebble's `cmd/pebble-challtestsrv`, a `package main` that cannot be imported,
so the test file restates those two handlers (same paths, JSON fields and status codes) on top of
the same library calls. The real binary is what the lab runs.

cert-manager also ships a conformance suite for DNS-01 solvers
(`github.com/cert-manager/cert-manager/test/acme`). It is not wired in here: it needs downloaded
control plane binaries (etcd and kube-apiserver), and it checks DNS through an authoritative
server, which the test server is not. See `main_test.go` in
[cert-manager/webhook-example](https://github.com/cert-manager/webhook-example) for how to run it
against a solver of your own.

## Build it and load it into kind

```sh
docker build -t lab-dns-webhook:dev .
kind load docker-image lab-dns-webhook:dev --name <your cluster>
```

The Dockerfile runs `go vet` and the tests before it builds, so a failing test fails the build.

## Deploy

cert-manager must already be installed (the serving certificate and the APIService's CA come from
it).

```sh
kubectl apply -f deploy/
```

| File | What it is |
|---|---|
| `00-namespace.yaml` | the namespace `lab-dns-webhook` |
| `10-serviceaccount.yaml` | the webhook's identity |
| `20-rbac.yaml` | the webhook may read the request header CA and delegate authorization; cert-manager may call the solver |
| `30-pki.yaml` | a self-signed Issuer, a CA, a CA Issuer and the serving Certificate |
| `40-deployment.yaml` | the Pod: not root, read-only root, no capabilities, HTTPS on 8443 |
| `50-service.yaml` | port 443 to the Pod's 8443 |
| `60-apiservice.yaml` | registers `v1alpha1.acme.lab.example`; cainjector fills in the CA |

The Deployment names the published image. To run the one you built and loaded instead:

```sh
kubectl -n lab-dns-webhook patch deployment lab-dns-webhook --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"lab-dns-webhook:dev"},{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
```

Healthy looks like this:

```
$ kubectl get apiservice v1alpha1.acme.lab.example
NAME                        SERVICE                           AVAILABLE   AGE
v1alpha1.acme.lab.example   lab-dns-webhook/lab-dns-webhook   True        21s
```

## Try the solver by hand

A cluster admin can send the same request cert-manager sends, without an Issuer:

```sh
cat > payload.json <<'EOF'
{"apiVersion":"webhook.acme.cert-manager.io/v1alpha1","kind":"ChallengePayload",
 "request":{"uid":"by-hand","action":"Present","type":"dns-01","dnsName":"z.lab.example","key":"hello",
  "resolvedFQDN":"_acme-challenge.z.lab.example.","resolvedZone":"lab.example.","resourceNamespace":"default",
  "config":{"managementURL":"http://challtestsrv.pebble.svc.cluster.local:8055"}}}
EOF
kubectl create --raw /apis/acme.lab.example/v1alpha1/challtestsrv -f payload.json
```

The answer ends `"response":{"uid":"by-hand","success":true}`, and the webhook logs one line:

```
solver.go:116] "present ok" fqdn="_acme-challenge.z.lab.example." zone="lab.example." keyLength=5
```

Change `"action"` to `"CleanUp"` and send it again to remove the record.

## When it does not work

What the Challenge's `status.reason` says (`kubectl describe challenge`), as seen with cert-manager
v1.21.3. While any of these holds, the Challenge stays `pending` and cert-manager retries with a
growing delay (5 s, 20 s, 40 s, 80 s, ...).

| Reason | Cause |
|---|---|
| `calling the challenge test server: Post "http://...:8055/set-txt": dial tcp: lookup ... no such host` | `managementURL` is wrong; the text is this solver's own error |
| `the server is currently unable to handle the request (post challtestsrv.acme.lab.example)` | no webhook Pod is ready; the APIService shows `False (MissingEndpoints)` |
| `challtestsrv.acme.lab.example is forbidden: User "system:serviceaccount:cert-manager:cert-manager" cannot create resource "challtestsrv" in API group "acme.lab.example" at the cluster scope` | the domain-solver ClusterRoleBinding is missing, or names another ServiceAccount |
| the same `is forbidden` text with a misspelled resource or group in it | `solverName` or `groupName` is wrong in the Issuer: RBAC refuses the name before anything can say it does not exist |

One trap: a Challenge keeps the solver settings it was created with. If you fix the Issuer while a
Challenge is failing, cert-manager makes a new Challenge and deletes the old one, and the old one's
clean up runs with the old, broken settings. It fails (`Error cleaning up challenge: ...`), the old
Challenge keeps its finalizer, and the new one waits behind it. Remove the finalizer from the old
Challenge to let go of it:

```sh
kubectl patch challenge <old name> --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'
```

A stopped webhook or a missing RBAC binding is different: nothing in the Challenge changes, so it
recovers by itself at its next retry.
