# ntctl

`ntctl` installs and operates the NeuralTrust platform on Kubernetes. It is a
single binary with Helm built in, so you need neither `helm` nor `kubectl`.

It installs the same [`neuraltrust-platform`](https://github.com/NeuralTrust/neuraltrust-platform)
chart you would install with Helm, from the same values. You can switch between
`ntctl` and plain `helm` at any time.

## Install

```sh
curl -sSfL https://raw.githubusercontent.com/NeuralTrust/ntctl/main/install.sh | sh
```

The script downloads the release for your OS and architecture and checks it
against `checksums.txt`. If [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
is installed, it also verifies that `checksums.txt` was signed by this
repository's release workflow. Set `NTCTL_REQUIRE_SIGNATURE=1` to make that
mandatory.

Prefer to inspect first? Download the script, read it, then run it with `sh`.
On Windows, download `ntctl_windows_amd64.zip` from
[Releases](https://github.com/NeuralTrust/ntctl/releases). With Go installed:

```sh
go install github.com/NeuralTrust/ntctl/cmd/ntctl@latest
```

## Quick start

You need:

- a Kubernetes cluster your kubeconfig points at;
- the tokens for your products: sign in with `ntctl login`, or copy them from
  the NeuralTrust console;
- either the registry key file NeuralTrust sent you, or your own registry with
  the images mirrored into it.

```sh
ntctl install
```

On a terminal, `ntctl install` asks for what it needs. Each question has a
default, so pressing Enter takes a working choice:

| Question | Default | Alternative |
|---|---|---|
| Where the control plane runs | NeuralTrust hosts it (hybrid) | in this cluster (external), or in this cluster serving data planes elsewhere (saas); see [Deployment modes](#deployment-modes) |
| Base domain for the endpoints | (required) | |
| Platform | detected from the cluster | aws, gcp, azure, openshift, kubernetes |
| How the endpoints are exposed (asked only when the cluster has a Gateway API Gateway) | Ingresses; a Gateway when the cluster has no IngressClass and is not GKE | HTTPRoutes on one of the cluster's Gateways |
| Products | all | any of `trustgate`, `trustguard`, `dataPlane` |
| [Operational telemetry](#operational-telemetry) | No | Yes: TrustGate and TrustGuard send their own traces and metrics to NeuralTrust in hybrid installs, or to your in-cluster collector otherwise |
| Where images come from | the NeuralTrust registry, with your key file | your own registry: its address, and either a username and password or the nodes' own identity (ECR, Artifact Registry, ACR) |
| PostgreSQL | run it in the cluster | your own, as a connection string `postgres://user:password@host:5432/database?sslmode=require` |
| Redis | run it in the cluster | your own, as `redis://:password@host:6379`, or `rediss://` for TLS |
| Console tokens | from the console, when signed in | asked for each product you install (hybrid only) |
| The console's first admin (external and saas only) | (required) | an existing `onprem-superadmin` Secret, or your own named in `global.superadmin.existingSecret.name` |

Passwords, tokens and connection strings are typed with input hidden. They are
stored only in Kubernetes Secrets in the install namespace: never in a file, a
log, or the Helm release. Your own PostgreSQL must already have the user and
database; NeuralTrust creates its tables on first start.

Then `ntctl install`:

1. Checks the cluster against what this install needs: storage, ingress (or,
   with HTTPRoutes, that the Gateway exists and admits the namespace),
   capacity, and every Secret it reads.

   Capacity counts only the nodes each workload can run on: a GPU pool or a
   tainted system pool does not count for general workloads, and GPU workers
   need a node that matches their selector. It also subtracts what pods already
   running there request. Above 85% of the free capacity it warns, because a
   rolling upgrade needs room for an extra pod; above 100% it stops. A cluster
   with Cluster Autoscaler or Karpenter only gets a warning, since it adds
   nodes when pods are pending.
2. Shows the cluster and the choices it is about to apply, and asks for
   confirmation.
3. Creates the Secrets, installs the chart, waits until every workload is
   ready, and prints the DNS records to create.
4. Writes the values it used to `neuraltrust-values.yaml`. The file holds no
   credentials. Pass it to `helm` or your GitOps tool if you move away from
   `ntctl`.

### Adding a product later

Run `ntctl install` again. When the namespace already has the install, it
finds that before asking anything and adds products to it instead:

```sh
ntctl install                          # asks which products to add
ntctl install --products trustguard --yes
```

- It asks only which products to add, offering the ones not installed, and
  then their console tokens. Domain, platform, exposure, images, PostgreSQL,
  Redis and operational telemetry stay as the install has them.
- It stays on the installed chart version unless you pass `--version`.
- It checks the cluster as an upgrade does, so capacity counts only what is
  added. It then shows every resource that changes and asks before applying.
  If the change does not become ready, the last working revision is restored.
- When every product you chose is already installed, it says so and changes
  nothing, so running the same command twice is safe. Without a terminal it
  adds only what `--products` names.
- `--domain`, `--platform`, `--gateway`, `--image-registry` or `--mode` with a
  value that differs from the install, or `-f` values that do, are refused
  rather than applied. Change those with `ntctl upgrade`. A registry key is not
  needed, since the install keeps its pull settings.
- An install that failed, or was interrupted, gets nothing added: it says how
  to recover first (`ntctl doctor`, then `ntctl upgrade`).
- When `neuraltrust-values.yaml` (or `--save-values`) is still there, it is
  updated to match.

`ntctl upgrade --products` does the same without questions. See
[Adding or removing a product](#adding-or-removing-a-product).

### Without questions

Anything you set on the command line, in a `-f` values file, or in the
environment is not asked. `--yes` takes the defaults for the rest and skips the
confirmation, for CI. Operational telemetry then stays off unless you pass
`--operational-telemetry=true`:

```sh
export NT_TRUSTGATE_CONFIG_SYNC_TOKEN=...   NT_TRUSTGATE_ENROLMENT_TOKEN=...
export NT_TRUSTGUARD_CONFIG_SYNC_TOKEN=...  NT_TRUSTGUARD_ENROLMENT_TOKEN=...
export NT_POSTGRES_URL='postgres://user:password@db.example.com:5432/neuraltrust?sslmode=require'
export NT_REDIS_URL='rediss://:password@cache.example.com:6380'
export NT_REGISTRY_USERNAME=robot NT_REGISTRY_PASSWORD=...
# external and saas installs only:
export ONPREM_SUPERADMIN_EMAIL=admin@example.com ONPREM_SUPERADMIN_PASSWORD=...

ntctl install --yes --domain platform.example.com \
  --image-registry registry.example.com/neuraltrust
```

| Setting | Flag | Environment |
|---|---|---|
| NeuralTrust registry key file | `--registry-key` | `NT_REGISTRY_KEY_FILE` |
| Your own registry | `--image-registry`, plus `--image-registry-node-auth` when nodes authenticate themselves | `NT_REGISTRY_USERNAME`, `NT_REGISTRY_PASSWORD` |
| Your own PostgreSQL | | `NT_POSTGRES_URL` |
| Your own Redis | | `NT_REDIS_URL` |
| Where the control plane runs | `--mode` | |
| The console's first admin (external and saas) | | `ONPREM_SUPERADMIN_EMAIL`, `ONPREM_SUPERADMIN_PASSWORD` |

Connection strings and passwords are environment-only, so they stay out of
shell history.

The PostgreSQL and Redis questions apply to hybrid installs, which share one
database user and one Redis. In external mode every service has its own
database user: configure that with `-f`, following
`values-managed-datastores.yaml.example` in the chart repository.

ntctl names the Secrets it creates for your datastores the way the chart's
examples do: `external-postgresql` (key `POSTGRES_PASSWORD`) and
`external-redis` (`REDIS_HOST`, `REDIS_PORT`, `REDIS_USERNAME`,
`REDIS_PASSWORD`, `REDIS_TLS`). Installs made by earlier versions keep their
`ntctl-postgresql` and `ntctl-redis` Secrets, since an upgrade reuses the values
and renames nothing.

### When only you may create Secrets

With `--secrets-mode print`, `install` asks and checks the cluster as usual,
then changes nothing. It saves the values and prints the `kubectl` commands
that create the Secrets, with a `<…>` placeholder for every password, token and
key. Hosts, ports and user names are printed as they are. Neither the console
nor you are asked for a token, and the PostgreSQL connection string may leave
out the password:

```sh
ntctl install --secrets-mode print --domain platform.example.com
# run the printed kubectl commands, filling in each placeholder, then:
ntctl install -f neuraltrust-values.yaml
```

The second run finds the Secrets in place, keeps them, and installs.

### Signed in to the console

`ntctl login` signs you in to the NeuralTrust console in the browser, where you
also choose the organization. After that `ntctl install` gets the TrustGate and
TrustGuard tokens itself, so the console UI is not needed:

```sh
ntctl login                 # --region us for app.us.neuraltrust.ai
ntctl install
```

For each product, `install` offers the organization's existing private
instances or creates a new one, after you confirm the install. It then records
the instance's endpoint in the console. To choose without questions, name the
instance:

```sh
ntctl install --yes --domain platform.example.com --trustgate-instance edge-1
```

For each instance, an instance already named like the one install would
create (the domain, by default) is used instead of creating a second one, so
running an install again after a failure keeps one instance. A name shared by
several instances is refused; name one by its ID.

The tokens go straight into the Kubernetes Secrets, as when you type them in.
They are valid only at the control plane of the console that issued them, so
the data plane dials that one: a console other than a region's (a dev or
self-hosted console) points the install at its own control plane
(`global.controlPlane.domain`), and the summary shows it as `Connects:`.
The sign-in is saved in `~/.config/ntctl/credentials.json`, which only you can
read. It is renewed as needed until it expires or you run `ntctl logout`. Tokens
in the environment still take precedence.

This needs a console that offers CLI sign-in. Without one, `ntctl login` says so
and everything else works as described above.

## Deployment modes

`--mode` (or `global.deploymentMode` in your values, or the first question)
says where the control plane runs:

| Mode | This cluster runs | Tokens from NeuralTrust | Asks for |
|---|---|---|---|
| `hybrid` (default) | the data plane; NeuralTrust hosts the control plane | yes | PostgreSQL and Redis (shared by every service) |
| `external` | everything, with its own console; nothing leaves the cluster | no | the console's first admin |
| `saas` | a control plane, with its own console, that data planes elsewhere connect to | no | the console's first admin |

In external and saas installs every service has its own PostgreSQL role, so
your own PostgreSQL and Redis are configured with `-f`, following
`values-managed-datastores.yaml.example` in the chart repository. After a saas
install, ntctl prints the DNS records data planes dial (`databridge.`,
`agentgateway-configsync.`, `trustguard-configsync.`) and the steps to connect
one. The mode of an install cannot change in place.

## Commands

| Command | What it does |
|---|---|
| `ntctl install` | Asks what it needs, checks the cluster, creates the Secrets, installs, and prints the DNS records. Run again on an existing install, it adds products |
| `ntctl upgrade` | Upgrades to the latest (or `--version`) chart, keeping the install's values: shows the release notes and every change first, stops on unsafe ones, rolls back if it does not become ready |
| `ntctl rollback` | Returns an install to an earlier revision (`--to N`), showing what changes first |
| `ntctl history` | The revisions of an install |
| `ntctl adopt` | Checks that an install made with plain `helm` can be managed with ntctl, changing nothing (`--chart` for air-gapped clusters) |
| `ntctl uninstall` | Removes an install; its volumes and the Secrets ntctl created are kept unless you pass `--delete-data` / `--delete-secrets` |
| `ntctl status` | Workload readiness, pods that need attention, and DNS records |
| `ntctl doctor` | Says what is wrong with an install and how to fix it (`--output json`) |
| `ntctl support-bundle` | Writes a redacted `.tar.gz` for NeuralTrust support |
| `ntctl preflight` | Runs the install or upgrade checks without changing anything (`--for upgrade`, `--output json`) |
| `ntctl render` | Prints the manifests an install would apply, as `helm template` does; `--output-dir` also writes the values |
| `ntctl version` | The `ntctl` version and the latest chart version |
| `ntctl login` / `logout` | Sign in to the console in the browser, or sign out and end the session |
| `ntctl auth status` | Who is signed in, to which console, and the selected organization |
| `ntctl orgs list` | The organizations you belong to |
| `ntctl config get` / `set org` / `set region` | Show or change the organization and region |
| `ntctl trustgate list` / `create <name>` | The organization's private TrustGate instances (`trustguard` for TrustGuard) |

Flags every command accepts:

- `--context`, `--kubeconfig`, `--namespace` (default `neuraltrust`), `--release` (default `neuraltrust-platform`).

Flags for `install`, `upgrade`, `preflight` and `render`:

- `-f values.yaml`: any chart value, as with `helm -f`. Repeat it to layer files.
- `--domain`, `--platform`, `--products`. On `upgrade`, `--products` only
  turns products on and the others keep their state; `--remove-products`
  turns products off. See [Adding or removing a product](#adding-or-removing-a-product).
- `--gateway <namespace>/<name>[:<listener>]`: expose the endpoints as Gateway
  API HTTPRoutes attached to that existing Gateway instead of Ingresses. The
  Gateway terminates TLS, and its DNS must cover the domain's names, including
  `*.llm.` and `*.mcp.`. Several installs can share one Gateway, each with its
  own `--domain`.
- `--operational-telemetry=true|false`: turn [operational telemetry](#operational-telemetry)
  on or off. Off by default on install; an upgrade without it keeps the
  install's setting.
- `--version`: the chart version.
- `--chart`: a local chart directory or `.tgz`, for air-gapped clusters.
- `--ignore-capacity`: install even when the cluster is too small, as on a test
  cluster. Pods that do not fit stay Pending.

Values are layered like Helm's: generated defaults, then `-f` files, then
explicit flags such as `--domain`.

Run `ntctl <command> -h` for every flag.

## Upgrading

```sh
ntctl upgrade --dry-run     # what would change, then stop
ntctl upgrade
```

`upgrade` keeps the values the install was made with. Before it changes
anything:

1. It runs the preflight checks.
2. It shows the chart's release notes between your version and the target, and
   flags releases to read before upgrading.
3. It asks the cluster to render the upgrade in a dry run, and lists every
   resource that is added, changed or removed (`--show-diff` for the full
   diff). Secret values are compared but never shown.

It stops, changing nothing, when the upgrade would need something Kubernetes
cannot do in place:

- a new selector on a Deployment, DaemonSet or StatefulSet;
- a new StatefulSet service name or volume template;
- a new storage class or access mode on a volume;
- removing a Secret, a volume or a StatefulSet;
- removing a key from a Secret.

Pass `--allow-breaking` only when you accept that those resources are deleted
and recreated. If the upgrade does not become ready, the last working revision
is restored.

When an install or upgrade did not become ready, `ntctl doctor` says why. Once
the cause is fixed, `ntctl upgrade` applies the failed revision again, even
when nothing in the values changed. An install or upgrade that was interrupted
(Ctrl+C, a lost connection) leaves the release locked: `ntctl rollback`
clears an interrupted upgrade, and `ntctl uninstall` an interrupted install.

### Adding or removing a product

```sh
ntctl upgrade --products trustguard --version <installed version> --dry-run
ntctl upgrade --products trustguard --version <installed version>
```

`--products` on `upgrade` turns the products it names on, and every other
product keeps running as it is. A product that already runs is left alone, so
naming one never removes another. `--version` keeps the installed chart; leave
it out to upgrade the chart in the same step.

The new product's token Secrets are created as `install` creates them: from
`NT_TRUSTGUARD_CONFIG_SYNC_TOKEN` and `NT_TRUSTGUARD_ENROLMENT_TOKEN` (or the
`NT_TRUSTGATE_*` pair), from the console when you are signed in, or asked for
with input hidden. Secrets already in place are kept, and a dry run asks for
nothing. If the install's values name Secrets of their own for that product,
those names are kept and ntctl creates nothing for them.

To turn a product off, name it in `--remove-products`. Its workloads are
deleted; the upgrade lists them first and asks before applying.

```sh
ntctl upgrade --remove-products trustgate --version <installed version>
```

To go back later:

```sh
ntctl history
ntctl rollback            # the newest earlier revision that worked
ntctl rollback --to 3
```

An install made with plain `helm` needs no migration. `ntctl adopt` checks that
it re-renders exactly as deployed, then writes its values, without anything that
looks like a secret, to `neuraltrust-values.yaml`. It changes nothing in the
cluster, and later upgrades keep the release's own values and Secret names.

## Operational telemetry

With operational telemetry on, TrustGate and TrustGuard export their own traces
and metrics: server spans, and request rate, errors and latency. Where they go
depends on the deployment mode:

- **hybrid** (the default): to NeuralTrust, through the install's egress
  collector and with its enrolment credentials.
- **external**: to the collector in your own cluster. Nothing leaves your
  environment.

It is off unless you turn it on. We recommend turning it on in hybrid installs:
NeuralTrust support can then see the health of your gateway and help you
faster.

`ntctl install` asks, with No as the default; `--operational-telemetry=true` or
`=false` answers without the question. `ntctl upgrade` keeps the install's
setting. On a terminal, an upgrade of an install that never recorded a choice
asks once; with `--yes` or without a terminal it never turns it on.

To switch it on while troubleshooting, and off again afterwards, change nothing
else and stay on the installed chart version (`ntctl status` shows it):

```sh
ntctl upgrade --operational-telemetry=true  --version <installed version>
ntctl upgrade --operational-telemetry=false --version <installed version>
```

With plain `helm`:

```sh
helm upgrade neuraltrust-platform <chart> -n neuraltrust --version <installed version> \
  --reuse-values --set global.observability.operationalTelemetry=true
```

`ntctl status` and `ntctl doctor` show the current setting. The values file
ntctl saves records it as `global.observability.operationalTelemetry`.

## When something is wrong

```sh
ntctl doctor
```

`doctor` works on any install, including one made with plain `helm`. It checks
the release against the cluster: storage, ingress, and every Secret the deployed
manifest reads. Then it looks at what is failing at runtime, and gives a code
and a fix for each finding:

| Code | Finding |
|---|---|
| `NT-DR-001` | The release failed, or an install or upgrade was interrupted |
| `NT-DR-010` | Images do not pull, grouped by cause: credentials refused, image or tag missing, registry unreachable |
| `NT-DR-011` | A container cannot start because a Secret or ConfigMap is missing |
| `NT-DR-012` | A container keeps crashing; its last log lines are shown |
| `NT-DR-013` | Pods cannot be scheduled, with the scheduler's reason |
| `NT-DR-014` | A container was killed for exceeding its memory limit |
| `NT-DR-015` | A container runs but never becomes ready; the probe's failures and the container's last errors are shown, with a fix for causes it recognises (a config-sync token from another control plane, refused database credentials, an untrusted certificate) |
| `NT-DR-020` | A volume is not bound |
| `NT-DR-021` | A Job failed, such as a database bootstrap hook; its last log lines are shown |
| `NT-DR-022` | Ingresses have no address yet |
| `NT-DR-023` | HTTPRoutes are not accepted by their Gateway, with the controller's reason |
| `NT-PF-*` | The preflight checks, against what is deployed |

If you need help, send NeuralTrust support a bundle:

```sh
ntctl support-bundle
```

The bundle is a `.tar.gz` holding:

- the doctor report and versions;
- the release's values, manifest and history;
- the namespace's resources and events;
- node capacity;
- the last 500 lines of each container's log (`--log-lines`).

It never includes a Secret's value; only Secret and key names. Passwords,
tokens, keys, connection strings and JWTs are replaced with `[REDACTED]` in
values, ConfigMaps, environment variables and logs. Any text equal to a
credential stored in a Secret of the namespace is masked wherever it appears.
The `README.md` inside lists every rule. Look through the archive before
sending it:

```sh
tar -tzf ntctl-support-*.tar.gz
```

## Starting over

```sh
ntctl uninstall                                  # asks you to type the namespace
ntctl uninstall --yes --delete-data --delete-secrets
```

`uninstall` removes the release and every resource it created. Your volumes
and the Secrets ntctl created stay unless you ask, and so does the namespace.
Private TrustGate and TrustGuard instances stay in the console; delete them
there if you no longer need them.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error |
| 2 | Unknown command |
| 3 | Checks failed (`preflight` or `doctor`); nothing was changed |

## Building

```sh
make build   # bin/ntctl
make test
make lint
```

`make test-e2e` installs on a real cluster, such as a local
[kind](https://kind.sigs.k8s.io/) cluster, against an in-memory console:

```sh
kind get kubeconfig --name kind > /tmp/kind.kubeconfig
NTCTL_E2E_KUBECONFIG=/tmp/kind.kubeconfig make test-e2e
```

## License

[Apache 2.0](LICENSE)
