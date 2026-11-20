# cert-manager-demo

Field Guide 09's demo image: one static binary, `/cert-manager-demo`, Go's standard library only. It
does the three things the book needs done with a certificate: serve HTTPS from one, call a server with
one, and say what is in one.

`serve` and `call` read their files again for every connection, so a Secret that cert-manager renews is
used with no restart. No command ever prints key material. Times are UTC, in RFC 3339.

## serve

```bash
cert-manager-demo serve -cert /tls/tls.crt -key /tls/tls.key -ca /tls/ca.crt
```

| Flag | Default | |
|---|---|---|
| `-addr` | `:8443` | HTTPS listen address |
| `-health-addr` | `:8080` | Plain HTTP, `/healthz` only, for probes |
| `-cert`, `-key` | | The server's certificate and key files |
| `-ca` | | The CA file client certificates are checked against |
| `-client-auth` | `require` | `none`: ask for no client certificate. `request`: a client may come without one, but one it sends must verify. `require`: no certificate, no answer |

Every path answers with the same lines: who called, then the certificate this connection was served.

```
client: CN=web
client dns names: web.demo.lab.example
client uris: -
client issuer: CN=Lab Issuer CA
server: CN=www.lab.example
server dns names: www.lab.example
server issuer: CN=Lab Issuer CA
server serial: 5a0c1e9b7d3f42a1
server not after: 2026-11-21T03:28:16Z
```

A client with no certificate gets one line, `client: no certificate`, in place of the four.

Its log says which certificate it serves whenever that changes, one line per request, and one line per
failed handshake:

```
serving: serial=5a0c1e9b7d3f42a1 notAfter=2026-11-21T03:28:16Z
request: GET / client: CN=web
http: TLS handshake error from 10.244.1.7:41234: tls: client didn't provide a certificate
```

## call

```bash
cert-manager-demo call -url https://www.demo.svc:8443/ -cert /tls/tls.crt -key /tls/tls.key -ca /tls/ca.crt
```

| Flag | Default | |
|---|---|---|
| `-url` | | The URL to GET |
| `-cert`, `-key` | none | The client's certificate and key files. Without them, no certificate is sent |
| `-ca` | the system's roots | The CA file the server is checked against |
| `-servername` | the URL's host | The name to check the server's certificate for |
| `-every` | `0` | Call again after this long, for ever. `0` calls once and exits 1 if the call fails |
| `-timeout` | `5s` | How long one call may take |

One line per call. A call that worked prints the server certificate's serial, issuer and end, and the
first line of the answer:

```
ok serial=5a0c1e9b7d3f42a1 issuer="CN=Lab Issuer CA" notAfter=2026-11-21T03:28:16Z body="client: CN=web"
```

A call that failed prints `error:` and Go's own words, unchanged:

```
error: Get "https://www.demo.svc:8443/": remote error: tls: certificate required
```

## show

```bash
cert-manager-demo show -ca ca.crt tls.crt
kubectl get secret www-tls -o jsonpath='{.data.tls\.crt}' | base64 -d | cert-manager-demo show
```

It reads PEM from a file, or from stdin with no file or `-`, and prints every certificate in order,
then how many there were. With `-ca` it also checks the first certificate against that CA file, with
the rest as intermediates: `verify: ok`, or `verify:` and Go's error (and exit code 1).

```
certificate 1
  subject:     CN=www.lab.example
  issuer:      CN=Lab Issuer CA
  dns names:   www.lab.example, api.lab.example
  ips:         -
  uris:        -
  not before:  2026-11-20T03:28:16Z
  not after:   2026-11-21T03:28:16Z
  serial:      5a0c1e9b7d3f42a1
  ca:          false
  key usage:   digital signature, key encipherment
  ext usage:   server auth
  public key:  ECDSA 256 bits (P-256)
certificate 2
  ...
chain: 2 certificate(s)
verify: ok
```

A private key block is never read. `show` prints one line for it and goes on:

```
skipped: a private key block
```

## version

```
$ cert-manager-demo version
1.0.0
```

## Test and build

The tests use real TLS over loopback. They make their keys in memory and write them only to the test's
own temporary folder.

```bash
go vet ./... && go test ./...
docker build -t cert-manager-demo:dev .   # runs the same vet and tests first
```

The image is published by the workflow `cert-manager-demo-image` in this repository.
