# crossplane/

The examples of Field Guide 08, *Crossplane on kind*, from the Kubernetes Notes zine series. The book runs
Crossplane v2.4.3 (chart `crossplane` 2.4.3, with the `crossplane` CLI v2.5.0) on No. 00's kind lab, with
provider-kubernetes v1.4.1, provider-helm v1.5.0, provider-nop v0.6.0 and provider-sql v0.16.2, and the
functions patch-and-transform v0.11.2, go-templating v0.13.2, auto-ready v0.7.3, sequencer v0.6.1 and
environment-configs v0.9.2. The world outside the cluster is PostgreSQL 18.6, run by the CloudNativePG operator
1.30.1 (chart `cloudnative-pg` 0.29.1). It pins the tag `crossplane-1.0`. Crossplane writes nothing to Git,
and chapter 13's Flux only reads this repo over anonymous HTTPS, so the book needs no fork: clone it
read-only at the tag.

Every package and image is named by digest. No file here holds a password: the ones the book needs are made
on the cluster at the time, and its printouts show Secret keys, never values.

- `install/`: the sha256 sums of the two charts, the `crossplane` CLI, the `flux` CLI and the Flux Operator's
  install manifest.
- `values/`: the charts' values. `crossplane.yaml` is the book's install; `crossplane-day2.yaml` is the same
  file with chapter 12's changes; `cnpg.yaml` pins the operator's image by digest.
- `packages/`: the Providers and Functions, a file each, with the ManagedResourceActivationPolicy that turns
  on each provider's kinds (the book installs with nothing activated). Also the pieces chapter 2 breaks on
  purpose: an older provider-nop to upgrade from and a package name with a typo.
- `managed/`: chapter 3's first managed resources: provider-nop's timer and a stubborn one, provider-kubernetes
  Objects (a namespace, a quota, observe-only, management policies), its ProviderConfig and the RBAC it needs.
- `helm/`: chapter 4: provider-helm's ProviderConfig, its RBAC and one Release.
- `postgres/`: chapter 5's CloudNativePG Cluster `pg` and the Secret shape for its admin login.
- `sql/`: provider-sql against that Cluster: its ProviderConfig, a Database, Role, Schema, Grant, Extension
  and DefaultPrivileges, then chapter 6's adoption, narrowed grants, a blocked delete, a name clash and a
  ProviderConfig with the wrong password.
- `api/`: the book's own API, the namespaced composite resource `AppDatabase`: its
  CompositeResourceDefinition a state at a time (`xrd.yaml`, a second version, `spec.app`, a default
  Composition), an EnvironmentConfig and a team's limits.
- `compositions/`: the Composition a step at a time, `01-providerconfig.yaml` to `14-tuned.yaml`: first
  patch-and-transform and auto-ready, then go-templating, environment configs, required resources, the
  sequencer, a Usage that guards deletion, the book's own Go function, and the demo app. `sketch*.yaml` are a
  second Composition for chapter 8's selection rules.
- `xr/`: the requests teams hand in (`orders`, `billing`, a scratch one) and the ones that show how a
  Composition is picked.
- `render/`: what `crossplane composition render` needs to run a Composition on a laptop: an XR, the
  functions, and observed resources.
- `function/`: the book's function, `function-appdb-schemas`, in Go (its own README says what it does and how
  to test it). The `crossplane-function` workflow publishes it as a package; the book pins v1.0.1 by digest.
- `registry/`: an optional in-cluster registry, for a reader who builds the function and wants to install
  their own build.
- `demo-image/`, `demo/`: the demo app `crossplane-demo` (it reports the database, schema, role and row count
  it sees, and never prints a password), built once by the `crossplane-demo-image` workflow and pinned by
  digest, with its Deployment, Service and an HTTPRoute on No. 00's Gateway.
- `rbac/`, `teams/`, `protection/`: chapter 11: what the core needs to compose CloudNativePG Clusters and
  HTTPRoutes, a team's Roles, a second team and the ones that are refused, and Usages that block a delete.
- `day2/`: chapter 12: runtime configs for a function and a provider, an ImageConfig that rewrites a registry,
  the function's v1.0.0 to upgrade from, and an ObservedObjectCollection.
- `gitops/`: chapter 13: a second kind cluster (`kind.yaml`), the Flux Operator's FluxInstance, the source and
  one hub Kustomization (`flux/`), and the layers it delivers in order (`core/`, `postgres-operator/`,
  `packages/`, `configs/`, `api/`, `tenants/`), wired in `clusters/`. `tenants/v1` to `v4` and
  `clusters/gitops-v2` to `-v4` stand in for commits. `everything/` and `breaks/` are the wrong ways, shown
  failing.
- `breaks/`: chapter 14's failure gallery: a Composition with a step missing, a function that crashes, a
  second Function from the same package, a managed resource with no ProviderConfig, name clashes, a quota, a
  NetworkPolicy that cuts a provider off, and a schema change that does not fit.

The lab is a throwaway: one fixed Secret shape, plain local-path storage, a superuser login for provider-sql.
None of it is advice for production.
