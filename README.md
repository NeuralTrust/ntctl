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
| Base domain for the endpoints | (required) | |
| Platform | detected from the cluster | aws, gcp, azure, openshift, kubernetes |
| Products | all | any of `trustgate`, `trustguard`, `dataPlane` |
| Where images come from | the NeuralTrust registry, with your key file | your own registry: its address, and either a username and password or the nodes' own identity (ECR, Artifact Registry, ACR) |
| PostgreSQL | run it in the cluster | your own, as a connection string `postgres://user:password@host:5432/database?sslmode=require` |
| Redis | run it in the cluster | your own, as `redis://:password@host:6379`, or `rediss://` for TLS |
| Console tokens | from the console, when signed in | asked for each product you install |

Passwords, tokens and connection strings are typed with input hidden. They are
stored only in Kubernetes Secrets in the install namespace: never in a file, a
log, or the Helm release. Your own PostgreSQL must already have the user and
database; NeuralTrust creates its tables on first start.

Then `ntctl install`:

1. Checks the cluster against what this install needs: storage, ingress,
   capacity, and every Secret it reads.
2. Shows the cluster and the choices it is about to apply, and asks for
   confirmation.
3. Creates the Secrets, installs the chart, waits until every workload is
   ready, and prints the DNS records to create.
4. Writes the values it used to `neuraltrust-values.yaml`. The file holds no
   credentials. Pass it to `helm` or your GitOps tool if you move away from
   `ntctl`.

### Without questions

Anything you set on the command line, in a `-f` values file, or in the
environment is not asked. `--yes` takes the defaults for the rest and skips the
confirmation, for CI:

```sh
export NT_TRUSTGATE_CONFIG_SYNC_TOKEN=...   NT_TRUSTGATE_ENROLMENT_TOKEN=...
export NT_TRUSTGUARD_CONFIG_SYNC_TOKEN=...  NT_TRUSTGUARD_ENROLMENT_TOKEN=...
export NT_POSTGRES_URL='postgres://user:password@db.example.com:5432/neuraltrust?sslmode=require'
export NT_REDIS_URL='rediss://:password@cache.example.com:6380'
export NT_REGISTRY_USERNAME=robot NT_REGISTRY_PASSWORD=...

ntctl install --yes --domain platform.example.com \
  --image-registry registry.example.com/neuraltrust
```

| Setting | Flag | Environment |
|---|---|---|
| NeuralTrust registry key file | `--registry-key` | `NT_REGISTRY_KEY_FILE` |
| Your own registry | `--image-registry`, plus `--image-registry-node-auth` when nodes authenticate themselves | `NT_REGISTRY_USERNAME`, `NT_REGISTRY_PASSWORD` |
| Your own PostgreSQL | | `NT_POSTGRES_URL` |
| Your own Redis | | `NT_REDIS_URL` |

Connection strings and passwords are environment-only, so they stay out of
shell history.

The PostgreSQL and Redis questions apply to hybrid installs, which share one
database user and one Redis. In external mode every service has its own
database user: configure that with `-f`, following
`values-managed-datastores.yaml.example` in the chart repository.

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

The tokens go straight into the Kubernetes Secrets, as when you type them in.
The sign-in is saved in `~/.config/ntctl/credentials.json`, which only you can
read. It is renewed as needed until it expires or you run `ntctl logout`. Tokens
in the environment still take precedence.

This needs a console that offers CLI sign-in. Without one, `ntctl login` says so
and everything else works as described above.

## Commands

| Command | What it does |
|---|---|
| `ntctl install` | Asks what it needs, checks the cluster, creates the Secrets, installs, and prints the DNS records |
| `ntctl upgrade` | Upgrades to the latest (or `--version`) chart, keeping the values the install was made with; rolls back if it does not become ready |
| `ntctl status` | Workload readiness, pods that need attention, and DNS records |
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
- `--domain`, `--platform`, `--products`.
- `--version`: the chart version.
- `--chart`: a local chart directory or `.tgz`, for air-gapped clusters.

Values are layered like Helm's: generated defaults, then `-f` files, then
explicit flags such as `--domain`.

Run `ntctl <command> -h` for every flag.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error |
| 2 | Unknown command |
| 3 | Preflight checks failed; nothing was changed |

## Building

```sh
make build   # bin/ntctl
make test
make lint
```

## License

[Apache 2.0](LICENSE)
