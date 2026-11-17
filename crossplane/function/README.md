# crossplane/function/

`function-appdb-schemas`, the composition function of Field Guide 08, *Crossplane on kind*, written in Go on
`function-sdk-go` v0.7.1. Chapter 10 reads it, tests it, renders with it and puts it in the book's Composition
in place of a template. The book installs the published package by tag and digest
(`../packages/function-appdb-schemas.yaml`), so a reader never has to build it.

## What it does

It reads `spec.schemas` of an `AppDatabase` (a list of `{name, access}`, `access` being `read` or `write`) and,
for each entry, adds three provider-sql resources to the desired state:

| Composition resource name | Kind (`postgresql.sql.m.crossplane.io/v1alpha1`) | Object name | What it asks for |
|---|---|---|---|
| `schema-<name>` | `Schema` | `<xr>-<name>` | the schema, in the database `<xr>`, owned by the role the provider logs in as |
| `grant-<name>` | `Grant` | `<xr>-<name>-grant` | `USAGE` for the role `<xr>-app`, and `CREATE` when the access is `write` |
| `defpriv-<name>` | `DefaultPrivileges` | `<xr>-<name>-tables` | `SELECT` on tables `postgres` makes there later, and `INSERT`, `UPDATE`, `DELETE` for `write` |

A `_` in a schema's name becomes `-` in the object's name. Everything earlier steps put in the desired state is
kept. On the XR it writes:

- `status.schemas`: one `{name, ready}` an entry, `ready` when all three resources are observed with `Ready True`;
- the condition `SchemasValid`: `True Checked` when every name passed, `False InvalidSchemaName` beside a fatal
  result when one did not (longer than 63 bytes, not matching `^[a-z_][a-z0-9_]*$`, `public`,
  `information_schema`, anything starting `pg_`, a name of the input's `reserved` list, a name listed twice, an
  access that is neither `read` nor `write`). A refusal composes nothing;
- the condition `SchemasReady`: `True AllReady` or `False Waiting`, with the message `N of M schemas ready`;
- one Event a pass, `composed N schema(s)`.

It logs one line a call at the default level (`Composed schemas` or `Refused schemas`, with the XR as
`namespace/name` and the number of schemas), and one more with `--debug`.

## The input

```yaml
- step: schemas
  functionRef:
    name: function-appdb-schemas
  input:
    apiVersion: appdbschemas.fn.k8s-notes.example/v1beta1
    kind: Input
    providerConfig: orders     # optional: the provider-sql ProviderConfig; default, the XR's own name
    reserved: [admin]          # optional: more schema names to refuse
    break: panic               # optional, for the book's break pages only: panic or bare-response
```

The type is `input/v1beta1/input.go`; `go generate ./...` writes its CRD to `package/input/`, which travels in
the package as documentation and is never installed. The SDK decodes the input strictly: an unknown field is a
fatal result.

## Files

- `fn.go`: the function. `fn_test.go`: its tests, no cluster needed.
- `main.go`: the command line (`--insecure`, `--debug`, the address), as in `function-template-go`.
- `input/`: the input's Go type. `package/`: `crossplane.yaml` (the package's metadata) and the input's CRD.
- `Dockerfile`: the runtime image, one static binary on distroless, both base images by digest.

## Testing it

The module wants Go 1.26 or newer. With any Docker, in the Go image the package is built with:

```bash
docker run --rm --volume "$PWD":/fn:ro --workdir /fn --env GOTOOLCHAIN=local \
  docker.io/library/golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d \
  sh -c 'go vet ./... && go test -count=1 -cover ./...'
```

To try it in a pipeline with no cluster, run it from source (`go run . --insecure --debug`) and point
`crossplane composition render` at it with `../render/functions-dev.yaml`; chapter 10 does both.

## How it is published

`.github/workflows/crossplane-function.yaml`, run by hand with the tag to publish (`-f tag=v1.0.1`). It runs the
tests, builds the runtime image for linux/amd64 and linux/arm64, wraps each in a package with `crossplane xpkg
build --embed-runtime-image-tarball` (the CLI pinned by checksum), and pushes both under one tag to
`ghcr.io/<owner>/k8s-notes-examples/function-appdb-schemas` with `crossplane xpkg push`. The package is public
and needs no login to pull.

| Version | What changed |
|---|---|
| v1.0.0 | the first release: three resources a schema, `status.schemas`, `SchemasValid` only to refuse |
| v1.0.1 | `SchemasValid True` when the list passes, the condition `SchemasReady`, one log line a call |
