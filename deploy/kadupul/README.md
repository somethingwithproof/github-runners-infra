# Kadupul DigitalOcean runners

Kadupul uses a separate repository-scoped controller alongside the existing
somethingwithproof controller. The shared GitHub App is installed on both
accounts; installation IDs select a local upstream, and each upstream verifies
the original webhook HMAC and its own installation identity independently.
The router refuses unknown installations and preserves the signed body.

The Kadupul controller allowlists kadupulhq/kadupul, template, terraform,
.github, and rondi. The website repository is excluded. Public repositories
need both `ALLOWED_REPOSITORIES` and `ALLOWED_PUBLIC_REPOSITORIES` entries.
Public-run verification requests a repository-scoped installation token with
`administration:write` and `actions:read`; it does not request actions write.

Before a public job is persisted or provisioned, the controller reads GitHub's
workflow-run record. The run ID, repository names, numeric repository IDs, and
non-fork head repository must agree. For a pull-request run, at least one PR
association must exist and every PR head and base repository ID must match the
allowlisted repository. Missing data, external forks, unknown events, API
failures, and unavailable verification fail closed. Push, schedule, manual, and
same-repository pull-request events are eligible. Pull-request-target,
workflow-run, and comment events are rejected.

Repository workflows select `[self-hosted, Linux, X64, kadupul-do]` for trusted
events only when `DO_RUNNERS_ENABLED=true`. External fork PRs keep their
GitHub-hosted runner selection. These protections operate in the controller as
well as the workflows: a workflow label alone cannot authorize cloud capacity.

Each VM receives a single-job JIT runner configuration, never a cloud token,
App private key, or controller callback secret. The Kadupul template omits
Chef, provides Docker/Compose/buildx/gh and a writable tool cache, and verifies
the runner archive against the official release checksum. Docker and sudo are
root-equivalent; same-repository code must remain trusted. The VM powers off
after its job and the controller deletes it through ownership-tagged lifecycle
state. TTL reconciliation backs up completion-event cleanup.

The pool uses a dedicated sfo3 VPC and a firewall targeting
`runner-controller-kadupul`, with no inbound rules and HTTP/HTTPS/DNS outbound
access. Container builds need HTTP for their signed Debian package repositories;
the VM's own APT mirrors use HTTPS.
APT mirrors use HTTPS. The initial size is `s-8vcpu-16gb`, capped at 16 live
runners with 8 controller workers. Jobs beyond that limit remain in durable
pending state. Idle runner capacity is zero; the existing controller host stays
on. The VM must be deleted to end billing; power-off alone is insufficient.

Deployment configuration lives in `/etc/github-runners/kadupul-env` with mode
0600, state in `/var/lib/github-runners-kadupul`, and the private key remains on
the controller. See `env.example`; populate credentials from protected runtime
configuration, never from repository files. The separate systemd units are
`kadupul-webhook` and `installation-router`; Caddy forwards `/webhook` to
127.0.0.1:8082. The router sends approved installation identities to the legacy
controller on 8080 or Kadupul on 8081. Legacy callbacks retain their existing
8080 route.

Before enabling repository routing, dispatch `digitalocean-runner-smoke.yml`,
verify the actual runner contract, completion, and droplet deletion, and pilot
a same-repository PR. Fork denial and verification-failure paths are covered
by offline regression tests. Confirm cleanup in both durable state and the
DigitalOcean inventory. Queued GitHub jobs have no job execution timeout;
cancel smoke runs if provisioning fails.

Rollback routing by setting `DO_RUNNERS_ENABLED=false` and cancelling/re-running
queued self-hosted jobs. Re-running a historical workflow uses its original
workflow revision, so PR branches must include the routing change before a new
run can benefit. Drain active Kadupul runners before stopping its controller;
restore the reviewed Caddy backup to route only the original controller.

The original controller deployment predates the durable implementation. Before
the App was made public, its installation and exact repository boundary was
backported onto its deployed revision and tested. That operational hotfix is
on `fix/legacy-installation-boundary`; do not merge that old-source branch over
modern main. Rollback backups retain the matching original binary and protected
environment. An App installation does not itself grant provisioning permission.
