# vaultwarden-k8s-sync

Sync secrets from a **self-hosted [Vaultwarden](https://github.com/dani-garcia/vaultwarden)**
instance into **Kubernetes Secrets**, on a schedule, from inside the cluster.

It reads **secure-note** items (Bitwarden item type `2`) from a given Vaultwarden
**org collection**, parses `KEY=VALUE` / `export KEY=VALUE` lines out of each
note body, and upserts one Kubernetes `Secret` per target namespace.

## Why this exists (the ESO gap)

The [External Secrets Operator](https://external-secrets.io/) has a Bitwarden
provider — but it targets the **Bitwarden Secrets Manager** API.

**Vaultwarden does not implement Secrets Manager.** It implements the Bitwarden
**password-manager** API (vaults, collections, ciphers, secure notes). So:

- ❌ ESO's Bitwarden provider **cannot** talk to Vaultwarden.
- ❌ `bw serve` + a generic webhook provider is extra moving parts and state.
- ✅ The official **`bw` CLI** *does* speak the password-manager API.

This tool is the small, boring glue: a CronJob that shells out to `bw`, reads a
collection's secure notes, and writes Kubernetes Secrets. No ESO, no `bw serve`.

Nothing here is Osmose-specific — it works for **anyone self-hosting
Vaultwarden** who wants collection-backed Kubernetes Secrets.

## Architecture

```
                        in-cluster CronJob (every 15m)
   ┌──────────────────────────────────────────────────────────────┐
   │  vaultwarden-k8s-sync                                         │
   │                                                              │
   │   BW_PASSWORD / BW_CLIENTID / BW_CLIENTSECRET                 │
   │        (from the bootstrap Secret, applied out-of-band)      │
   │                     │                                        │
   │                     ▼                                        │
   │   ┌───────────────────────────────┐   HTTPS   ┌───────────┐  │
   │   │ internal/vaultwarden          │──────────▶│ Vaultwarden│ │
   │   │  shells out to `bw`:          │◀──────────│  server    │ │
   │   │   config server / login /     │  items    └───────────┘  │
   │   │   unlock / sync / list items  │                          │
   │   │   --collectionid <id>         │                          │
   │   │  filter .type==2 → .notes     │                          │
   │   │  ParseNotes(KEY=VALUE)        │                          │
   │   └───────────────┬───────────────┘                          │
   │                   │  map[string]string (values never logged) │
   │                   ▼                                          │
   │   ┌───────────────────────────────┐                          │
   │   │ internal/k8s (client-go)      │                          │
   │   │  in-cluster; upsert Secret    │                          │
   │   │  StringData, idempotent diff  │                          │
   │   └───────────────┬───────────────┘                          │
   └───────────────────┼──────────────────────────────────────────┘
                       │ create/update
        ┌──────────────┼───────────────┐
        ▼                              ▼
  Secret app-secrets            Secret app-secrets
  ns: app-prod                  ns: app-staging
  (labeled managed-by=          (labeled managed-by=
   vaultwarden-k8s-sync)         vaultwarden-k8s-sync)
```

### One sync run

1. `bw config server $VAULTWARDEN_SERVER`
2. `bw login --apikey` (only if not already authenticated)
3. `bw unlock --passwordenv BW_PASSWORD --raw` → session token
4. `bw sync`
5. For each mapping: `bw list items --collectionid <id> --session <s>`,
   keep `.type == 2`, take each `.notes` body
6. Parse `KEY=VALUE` lines, merge across notes
7. Upsert the target `Secret` (create or update, idempotent)

## Mapping model

Each mapping binds one **collection** to one **Secret**:

```json
[
  {"collectionId":"<uuid>","namespace":"app-prod","secretName":"app-secrets"},
  {"collectionId":"<uuid>","namespace":"app-staging","secretName":"app-secrets"}
]
```

All secure notes in a collection are merged into a single Secret. On key
collisions, later notes win.

### Note body format

Each secure note's **Notes** field is treated as dotenv-ish text:

```sh
# comments and blank lines are ignored
export DATABASE_URL=postgres://user:pass@host:5432/db?sslmode=require
API_TOKEN=YWJjZGVmZ2g=
GREETING=hello world
```

Parser rules:

- optional leading `export ` is stripped,
- blank lines and `#` comment lines are skipped,
- split on the **first** `=` only (so `=` in the value is fine),
- key is trimmed; value is taken verbatim (CRLF `\r` removed).

## Configuration

| Env var             | Flag              | Required | Description                                          |
| ------------------- | ----------------- | -------- | ---------------------------------------------------- |
| `VAULTWARDEN_SERVER`| `--server`        | yes      | Vaultwarden base URL.                                |
| `BW_PASSWORD`       | `--password`      | yes      | Master password used to unlock the vault.            |
| `BW_CLIENTID`       | `--client-id`     | no\*     | API-key client_id for `bw login --apikey`.           |
| `BW_CLIENTSECRET`   | `--client-secret` | no\*     | API-key client_secret.                               |
| `SYNC_MAPPINGS`     | `--mappings`      | yes\*\*  | JSON array of `{collectionId,namespace,secretName}`. |
| —                   | `--map`           | yes\*\*  | Repeatable `collectionID:namespace:secretName`.      |
| `SYNC_INTERVAL`     | `--sync-interval` | no       | Advisory only; the CronJob schedule is authoritative.|
| `BW_CLI`            | —                 | no       | Path to the `bw` binary (default `bw`).              |

\* API-key creds are required the first time (unauthenticated CLI); once a
profile is logged in, only `BW_PASSWORD` is needed to unlock.
\*\* Provide mappings via **either** `SYNC_MAPPINGS` **or** one/more `--map`.

**Sync interval:** there is no internal loop — one process run does exactly one
sync and exits. Cadence is owned by the CronJob `schedule` (default `*/15 * * * *`).

## Deploy

Manifests live in [`deploy/`](deploy/) and are Kustomize-able.

1. **Apply the bootstrap Secret out-of-band** (see the warning below):

   ```sh
   kubectl -n vaultwarden-sync create secret generic vaultwarden-bootstrap \
     --from-literal=BW_PASSWORD='...' \
     --from-literal=BW_CLIENTID='...' \
     --from-literal=BW_CLIENTSECRET='...'
   ```

2. **Edit** `deploy/cronjob.yaml` (`VAULTWARDEN_SERVER`, `SYNC_MAPPINGS`) and
   `deploy/rbac.yaml` (one Role + RoleBinding per target namespace).

3. **Apply** the rest:

   ```sh
   kubectl apply -k deploy/
   ```

4. **Trigger a one-off run** to verify:

   ```sh
   kubectl -n vaultwarden-sync create job --from=cronjob/vaultwarden-k8s-sync manual-1
   kubectl -n vaultwarden-sync logs job/manual-1 -f
   ```

## Build

```sh
make build          # local binary in ./bin (needs modules; run `make deps` once online)
make test           # runs the parser unit tests
make docker         # multi-stage image with the bw CLI baked in
```

> `go.sum` is not committed. Run `make deps` (`go mod tidy`) once in an
> environment with network access before the first build.

The runtime image installs the **Bitwarden CLI** via a pinned
`@bitwarden/cli@<version>` npm package (arch-agnostic). The Dockerfile also
documents the alternative release-tarball install (linux-x64 only) — see
comments in [`Dockerfile`](Dockerfile).

## Security notes

- **Least-privilege RBAC.** The ServiceAccount is granted a namespaced `Role`
  (not a ClusterRole) in each target namespace, limited to `secrets` with verbs
  `get, create, update, patch`. No `list/watch/delete`, no cluster-wide scope.
- **Bootstrap creds live *outside* Vaultwarden.** The syncer's own login is a
  cold-start dependency: it can't fetch its Vaultwarden credentials *from*
  Vaultwarden without a chicken-and-egg deadlock. Store the bootstrap Secret via
  sealed-secrets / SOPS / your cloud secret manager / a one-time admin apply —
  never in Vaultwarden, never in git. See
  [`deploy/bootstrap-secret.example.yaml`](deploy/bootstrap-secret.example.yaml).
- **Secret values are never logged.** Logs (structured JSON via `log/slog`)
  record which **keys** were created/changed/removed — never their values.
  Credentials are passed to `bw` via environment, never as CLI args.
- **Idempotent.** A run only writes when the desired data differs from what's in
  the cluster; unchanged Secrets are left untouched.
- **Hardened pod.** Runs as non-root with a read-only root filesystem, all
  capabilities dropped, and `RuntimeDefault` seccomp.
- **Exit codes.** Any failure exits non-zero so the Job/CronJob surfaces it.

## License

[MIT](LICENSE) © 2026 Paul Gabriel
