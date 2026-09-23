# AGENTS.md — Osmose deployment context

The `README.md` documents this tool generically. This file records how it's used
in the **Osmose k3s platform** (it's the secrets layer of that platform).

## Role in the platform

Secrets never live in git. They live in **Vaultwarden**
(`vaultwarden.services.osmose.co`) and this CronJob reconciles them into
Kubernetes:

- Reads the secure-note items named **`<svc>-env`** (one per service) from the
  collections **`Tech/kube-staging`** and **`Tech/kube-production`**.
- Writes one `<svc>-env` **Secret** into the app namespace (`app-staging` on the
  staging cluster, `app-prod` on prod).
- Workloads consume it via `envFrom: secretRef: <svc>-env`.
- **Stakater Reloader** (`autoReloadAll: true`, deployed by osmose-gitops)
  restarts any workload whose referenced Secret changes — so a Vaultwarden edit
  propagates without a manual rollout.
- Runs on a **15-minute schedule**. `osctl secrets sync --env <env>` triggers it
  immediately.

## Deployment

Deployed by Argo CD from **osmose-gitops**:
- staging → `apps/vaultwarden-sync.yaml` (path `deploy/`)
- prod → `envs/prod/vaultwarden-sync.yaml` (path `deploy-prod/`, which patches
  `SYNC_MAPPINGS` to the prod collection + namespace).

Bootstrap credentials (`BW_PASSWORD`, `BW_CLIENTID`, `BW_CLIENTSECRET`) come from
an **out-of-band** `vaultwarden-bootstrap` Secret (see
`deploy/bootstrap-secret.example.yaml`) — applied by hand, never in git.

## The bw-state PVC (don't drop it)

`deploy/pvc.yaml` persists the `bw` CLI state (HOME `/home/app`) across CronJob
runs. Without it, every run started unauthenticated, ran `bw login --apikey` with
a fresh **device identifier**, and Vaultwarden emailed a "New Device Logged In"
notification each time. With the PVC the syncer logs in once; later runs see `bw
status` already authenticated and only unlock — no new device, no email.

## Editing secrets

Edit the `<svc>-env` secure note in Vaultwarden (via the UI or `bw` /
`osmose-bw`), then let the CronJob sync (or `osctl secrets sync`). k8s `<svc>-env`
Secret naming and the `KEY=VALUE` note format are the contract with the
Deployments in osmose-gitops (`base/<svc>` + overlays). Hostnames in values are
**k8s Service DNS** (hyphens, e.g. `backend-redis`), never Swarm `stack_service`
names.

Related repos: **osmose-gitops** (deploys this, consumes the Secrets), **ai-tooling**
(`osctl secrets sync`, `osmose-bw`).
