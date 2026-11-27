# Multi-cloud GitHub Actions JIT Runners

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Quality Gate](https://sonarcloud.io/api/project_badges/measure?project=somethingwithproof_github-runners-infra&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=somethingwithproof_github-runners-infra)
[![Go minimum](https://img.shields.io/badge/Go_minimum-1.26.9-blue)](./go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/somethingwithproof/github-runners-infra/badge)](https://scorecard.dev/viewer/?uri=github.com/somethingwithproof/github-runners-infra)

**Languages:** **English** · [Deutsch](docs/i18n/de/README.md) · [Español](docs/i18n/es/README.md) · [Français](docs/i18n/fr/README.md) · [Português (BR)](docs/i18n/pt-BR/README.md) · [日本語](docs/i18n/ja/README.md) · [한국어](docs/i18n/ko/README.md) · [简体中文](docs/i18n/zh-CN/README.md) · [العربية](docs/i18n/ar/README.md) · [हिन्दी](docs/i18n/hi/README.md)

<details>
<summary>All 48 languages</summary>

**Tier 1:** **English** · [Deutsch](docs/i18n/de/README.md) · [Español](docs/i18n/es/README.md) · [Français](docs/i18n/fr/README.md) · [Italiano](docs/i18n/it/README.md) · [Português (BR)](docs/i18n/pt-BR/README.md) · [Nederlands](docs/i18n/nl/README.md) · [Русский](docs/i18n/ru/README.md) · [Українська](docs/i18n/uk/README.md) · [日本語](docs/i18n/ja/README.md) · [한국어](docs/i18n/ko/README.md) · [简体中文](docs/i18n/zh-CN/README.md) · [繁體中文](docs/i18n/zh-TW/README.md) · [العربية](docs/i18n/ar/README.md) · [हिन्दी](docs/i18n/hi/README.md) · [Polski](docs/i18n/pl/README.md) · [Türkçe](docs/i18n/tr/README.md) · [Svenska](docs/i18n/sv/README.md) · [Tiếng Việt](docs/i18n/vi/README.md) · [Bahasa Indonesia](docs/i18n/id/README.md)

**African:** [Afrikaans (ZA)](docs/i18n/af-ZA/README.md) · [አማርኛ (ET)](docs/i18n/am-ET/README.md) · [العربية (مصر)](docs/i18n/ar-EG/README.md) · [العربية (المغرب)](docs/i18n/ar-MA/README.md) · [Français (MA)](docs/i18n/fr-MA/README.md) · [Français (SN)](docs/i18n/fr-SN/README.md) · [Hausa (NG)](docs/i18n/ha-NG/README.md) · [Igbo (NG)](docs/i18n/ig-NG/README.md) · [Malagasy (MG)](docs/i18n/mg-MG/README.md) · [Kinyarwanda (RW)](docs/i18n/rw-RW/README.md) · [Soomaali (SO)](docs/i18n/so-SO/README.md) · [Kiswahili (KE)](docs/i18n/sw-KE/README.md) · [isiXhosa (ZA)](docs/i18n/xh-ZA/README.md) · [Yorùbá (NG)](docs/i18n/yo-NG/README.md) · [isiZulu (ZA)](docs/i18n/zu-ZA/README.md)

**Asian:** [বাংলা](docs/i18n/bn/README.md) · [فارسی](docs/i18n/fa/README.md) · [עברית](docs/i18n/he/README.md) · [मराठी](docs/i18n/mr/README.md) · [தமிழ்](docs/i18n/ta/README.md) · [తెలుగు](docs/i18n/te/README.md) · [ไทย](docs/i18n/th/README.md) · [اردو](docs/i18n/ur/README.md) · [Bahasa Melayu](docs/i18n/ms/README.md) · [Filipino](docs/i18n/fil/README.md)

**South Slavic:** [Bosanski](docs/i18n/bs/README.md) · [Hrvatski](docs/i18n/hr/README.md) · [Српски](docs/i18n/sr/README.md)

</details>

A Go controller that creates one-job, repository-scoped GitHub Actions runners
on DigitalOcean, AWS EC2, Google Compute Engine, Azure VMs, Vultr Cloud Compute,
or Akamai Cloud (Linode). GitHub webhook deliveries are persisted before
acknowledgement, provisioning is retried after transient failures or controller
restarts, and only controller-owned resources can be deleted.

A JIT registration is scoped to a repository, not bound to its triggering job.
Before registering a runner and again before allocating a VM, the controller
checks that the original job is still queued. API failures defer provisioning.
Reconciliation also retires runners whose original job has completed, but first
deregisters through GitHub, which rejects removal while another job is running.

When a JIT runner completes a different job, its VM and registration are cleaned
up first. The controller then checks the original job: only demand still queued
gets replacement capacity. Original-job completion is persisted independently and
wins over stale API observations. An unavailable API defers the demand decision;
it does not discard the queued job or allocate an unverified replacement.

## Security architecture

```text
GitHub workflow_job webhook
  -> verify HMAC, installation ID, delivery ID, private-repository allowlist
  -> durably record the job
  -> verify the exact job is still queued through GitHub's Actions API
  -> generate a repository-scoped, single-use GitHub JIT configuration
  -> create an ownership-tagged instance with the selected cloud provider
  -> runner executes at most one job and powers off
  -> workflow_job:completed schedules controller-owned deletion
  -> controller reconciles owned runners that exceed MAX_RUNNER_AGE
```

Cloud credentials and the GitHub App private key remain on the controller.
Runner user data contains only GitHub's single-use JIT configuration and public
artifact metadata. Before deletion, every provider adapter verifies the
controller ownership tag or label. Existing DigitalOcean state files are
automatically migrated from numeric `droplet_id` to provider-neutral string
`instance_id` values.

Before upgrading a DigitalOcean controller, stop the service and copy the state
snapshot, WAL, and lock metadata to protected durable storage. Verify that the
backup can be restored on a staging host before changing `COMPUTE_PROVIDER`.
Rollback means stopping the new controller, restoring the matching pre-upgrade
binary, configuration, snapshot, and WAL together, then confirming provider
ownership before restarting. Never run old and new controllers against the same
state or provider fleet concurrently.

The controller rejects public repositories by default. An explicit
`ALLOWED_PUBLIC_REPOSITORIES` subset can opt trusted repositories into run-origin
verification: GitHub's workflow-run record must show a non-fork head with the
same repository identity, and pull-request head/base repository IDs must match.
Fork PRs and unverified origins cannot provision runners. See
[the Kadupul deployment](deploy/kadupul/README.md) for the isolated rollout.
Self-hosted workflows still execute
repository code with root-equivalent Docker access on an isolated VM, so permit
only explicitly trusted repositories and same-repository code. Prefer private subnets with
egress through NAT and security groups or firewall rules with no inbound access.

## Supported providers

| `COMPUTE_PROVIDER` | Standard capacity | Interruptible capacity | Per-label size override (`LABEL_SIZE_MAP`) | Terraform module |
| --- | --- | --- | --- | --- |
| `digitalocean` | Droplet | Not available; `RUNNER_SPOT=true` fails closed | Supported (also accepts `DO_LABEL_SIZE_MAP`) | [`terraform/digitalocean`](terraform/digitalocean) |
| `aws` | EC2 On-Demand | EC2 one-time Spot, terminate on interruption | Supported (`AWS_INSTANCE_TYPE` override) | [`terraform/aws`](terraform/aws) |
| `gcp` | Standard VM | Spot VM, delete on preemption | Supported (`GCP_MACHINE_TYPE` override) | [`terraform/gcp`](terraform/gcp) |
| `azure` | Standard VM | Spot VM, delete eviction policy | Supported (`AZURE_VM_SIZE` override) | [`terraform/azure`](terraform/azure) |
| `vultr` | Cloud Compute | Not available; `RUNNER_SPOT=true` fails closed | Supported (`VULTR_PLAN` override) | [`terraform/vultr`](terraform/vultr) |
| `linode` | Akamai Cloud / Linode | Not available; `RUNNER_SPOT=true` fails closed | Supported (`LINODE_INSTANCE_TYPE` override) | [`terraform/linode`](terraform/linode) |

`RUNNER_SPOT` defaults to `false`. Spot instances can disappear at any time and
have no availability guarantee. A preempted GitHub job may need to be retried by
workflow policy; the lifecycle TTL remains the cleanup backstop.

DigitalOcean firewall posture is validated through its API before provisioning.
AWS security-group, GCP VPC-firewall, Azure subnet/NSG, Vultr firewall group,
and Linode Cloud Firewall ingress policies are managed through IaC (reference
modules live under [`terraform/`](terraform/README.md)). Keep runners private
and deny all unsolicited inbound traffic; enabling an external IP requires a
separate policy review.

## Prerequisites

- Go 1.26.9 or newer from a maintained Go release line
- A Linux controller host with Caddy or another TLS reverse proxy
- A GitHub App subscribed to `workflow_job` with repository Administration: write
  and Actions: read, scoped to the configured repository allowlist
- Least-privilege credentials for exactly one supported cloud provider
- A reviewed Chef installer SHA-256 and GitHub runner archive SHA-256
- Outbound HTTPS from runner subnets, directly or through NAT

`actions/runner` v2.336.0 also attempts the diagnostic request
`http://ssh.github.com/_dns` over TCP 80 when a job starts. The request is an
upstream connectivity probe, not required runner traffic, so this project keeps
the HTTPS-only policy and expects that probe to be denied. Suppress only the
corresponding known-denied alert. If local policy instead requires every
diagnostic to succeed, permit TCP 80 only through an egress proxy restricted to
the exact `ssh.github.com/_dns` destination; never allow general HTTP egress.
Validate the selected behavior from the runner subnet before deployment and
retain the firewall or proxy test as deployment evidence.

AWS uses the Go SDK default credential chain, GCP uses Application Default
Credentials, and Azure uses `DefaultAzureCredential`. Prefer workload identity
or a controller instance role over long-lived static credentials. The
DigitalOcean adapter reads `DIGITALOCEAN_TOKEN` because DigitalOcean's API client
does not provide an equivalent workload-identity chain.

## Common configuration

Create the GitHub App private-key file separately:

```bash
getent group webhook >/dev/null 2>&1 || sudo groupadd --system webhook
if id -u webhook >/dev/null 2>&1; then
  sudo usermod --append --groups webhook webhook
else
  sudo useradd --system --gid webhook --home /var/lib/github-runners --shell /usr/sbin/nologin webhook
fi
sudo install -d -o webhook -g webhook -m 0700 /etc/github-runners
sudo install -o webhook -g webhook -m 0600 github-app.pem /etc/github-runners/github-app.pem
```

Do not change the service account UID or GID underneath an existing state
directory.

Create `/etc/github-runners/env` with mode `0600`. Values below are examples,
not usable credentials or checksums:

```dotenv
APP_ID=123456
APP_INSTALLATION_ID=789012
APP_PRIVATE_KEY_FILE=/etc/github-runners/github-app.pem
WEBHOOK_SECRET=replace-with-at-least-32-random-bytes

COMPUTE_PROVIDER=aws
CONTROLLER_ID=primary
RUNNER_SPOT=false

ALLOWED_REPOSITORIES=example-org/private-repo,example-org/other-private-repo
ALLOWED_LABELS=self-hosted,chef,large
REQUIRED_LABEL=self-hosted
# Optional: comma-separated label=size overrides applied across all 6 providers.
# DigitalOcean also accepts the backward-compatible alias DO_LABEL_SIZE_MAP.
LABEL_SIZE_MAP=large=m7i.2xlarge

RUNNER_VERSION=2.336.0
RUNNER_SHA256=04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d
CHEF_INSTALLER_SHA256=replace-with-the-reviewed-installer-digest

RUNNER_GROUP_ID=1
WORKER_COUNT=4
MAX_LIVE_RUNNERS=20
MAX_ATTEMPTS=5
MAX_RUNNER_AGE=6h
# Optional: secret-free in-VM dead-man poweroff fuse in minutes (default 180; 0 disables).
RUNNER_DEADMAN_MINUTES=180
RUNNER_REGISTRATION_TIMEOUT=10m
LIVENESS_SETTLE_WINDOW=2m
LIVENESS_CONFIRMATIONS=3
CANCELLED_RUNNER_TTL=5m
STATE_FILE=/var/lib/github-runners/state.json
DELETED_RECORD_RETENTION=24h
CLOUD_INIT_PATH=/opt/github-runners/cloud-init/runner.yaml.tmpl
LISTEN_ADDR=:8080
```

`STATE_FILE` must live on durable storage. The controller also performs a
fail-safe provider sweep: controller-owned resources absent from that state are
reclaimed only after `MAX_RUNNER_AGE`.

`LABEL_SIZE_MAP` maps allowlisted workflow labels to provider-specific instance
sizes (for example `large=c-8,xlarge=c-16` on DigitalOcean, `large=m7i.2xlarge`
on AWS, `large=n2-standard-8` on GCP, `large=Standard_D8s_v5` on Azure,
`large=vhf-8c-16gb` on Vultr, or `large=g6-dedicated-8` on Linode). Every label
key must also appear in `ALLOWED_LABELS`, and size values are validated against
a strict character allowlist. Set either `LABEL_SIZE_MAP` or the DigitalOcean
alias `DO_LABEL_SIZE_MAP`, not both.

`RUNNER_DEADMAN_MINUTES` arms a secret-free transient `systemd-run` poweroff
timer inside each runner VM (default `180` minutes, `0` disables). No cloud API
credentials enter cloud-init user data. Because providers such as DigitalOcean,
Vultr, and Linode continue billing powered-off VMs until the instance is
destroyed, the dead-man fuse halts runaway execution while the controller's
completion handler and age-gated `MAX_RUNNER_AGE` sweep remain responsible for
destroying the instance and ending billing.

All allowlisted repositories must belong to the same GitHub App installation
owner. Installation tokens are restricted to exactly those repository names and
only the `administration: write` permission needed for runner management, plus
`actions: read` when public run-origin verification is enabled.

Do not derive an integrity value and execute the artifact in the same automated
step. Obtain runner digests from official release instructions and review the
Chef installer digest during an intentional upgrade.

### DigitalOcean

```dotenv
COMPUTE_PROVIDER=digitalocean
DIGITALOCEAN_TOKEN=read-from-your-secret-manager-at-deploy-time
DO_REGION=nyc3
DO_SIZE=s-4vcpu-8gb-amd
DO_IMAGE=ubuntu-24-04-x64
DO_VPC_UUID=private-vpc-uuid
DO_FIREWALL_ID=deny-all-inbound-firewall-id
# Optional: project UUID that runner droplets are assigned to for billing.
DO_PROJECT_ID=
```

Check the selected region's available sizes before deployment. Regional capacity
can change even when a size remains in the global catalogue. The AMD default
keeps 4 vCPUs, 8 GB RAM and 160 GB disk; the older `s-4vcpu-8gb` shape was rejected
in NYC3 with HTTP 422 on 2026-10-06. Set `DO_SIZE` explicitly for a different
available shape. After correcting an exhausted provisioning attempt, rerun its
queued workflow to issue fresh job demand rather than editing controller state.

Without `DO_PROJECT_ID`, runner droplets land in the account's default project.
When it is set, each new droplet is assigned to that project after creation; an
assignment failure is logged and does not fail the job.

Place runners in a dedicated VPC and configure the required Cloud Firewall with
no inbound rules. It must target the `runner-controller-${CONTROLLER_ID}` tag so
it applies as each tagged Droplet is created; the controller verifies both the
rules and tag before every create. Runner cloud-init also disables root and
password SSH access. For a custom-scope token, grant `droplet:create`,
`droplet:read`, `droplet:delete`, `tag:create`, `tag:read`, `vpc:read`, and
`firewall:read`. DigitalOcean also requires `regions:read`, `sizes:read`,
`actions:read`, and `image:read` with `droplet:create`. The controller only
reads the preconfigured Firewall; it does not need Firewall write scopes. When
`DO_PROJECT_ID` is set, also grant `project:assign_resource`; without it the
assignment is logged as failed and the droplet stays in the default project. Do
not grant unrelated account or project permissions.

### AWS EC2

```dotenv
COMPUTE_PROVIDER=aws
RUNNER_SPOT=true
AWS_REGION=us-west-2
AWS_AMI_ID=ami-pinned-id
AWS_INSTANCE_TYPE=m7i.xlarge
AWS_SUBNET_ID=subnet-private-id
AWS_SECURITY_GROUP_IDS=sg-egress-only-id
AWS_INSTANCE_PROFILE_ARN=
AWS_KEY_NAME=
AWS_EXTERNAL_IP=false
```

The adapter uses an idempotency token, ownership tags, one-time Spot requests,
termination interruption behavior, required IMDSv2, a one-hop metadata limit,
disabled instance-tag access through IMDS, and no public IP by default. Provide
NAT or another HTTPS egress path when `AWS_EXTERNAL_IP=false`. Omit the instance profile unless
runner jobs genuinely require AWS API access; attaching a profile grants its
permissions to untrusted workflow code.

The controller role needs narrowly scoped `ec2:RunInstances`,
`ec2:DescribeInstances`, `ec2:CreateTags`, and `ec2:TerminateInstances`, plus
`iam:PassRole` only when `AWS_INSTANCE_PROFILE_ARN` is set. Enforce required
controller tags in IAM conditions where possible.

### Google Compute Engine

```dotenv
COMPUTE_PROVIDER=gcp
RUNNER_SPOT=true
GCP_PROJECT_ID=runner-project
GCP_ZONE=us-central1-a
GCP_MACHINE_TYPE=n2-standard-4
GCP_SOURCE_IMAGE=projects/ubuntu-os-cloud/global/images/ubuntu-pinned-image
GCP_SUBNETWORK=projects/runner-project/regions/us-central1/subnetworks/runners
GCP_RUNNER_SERVICE_ACCOUNT_EMAIL=github-runner@runner-project.iam.gserviceaccount.com
GCP_EXTERNAL_IP=false
```

The adapter uses deterministic names, ownership labels, idempotent request IDs,
auto-deleted boot disks, and `DELETE` as the Spot termination action. Each VM is
attached to the explicitly configured dedicated runner service account with only
the `userinfo.email` OAuth scope. Grant that identity no IAM roles unless a
trusted workflow has an explicit, reviewed requirement. With
`GCP_EXTERNAL_IP=false`, the subnet must provide Cloud NAT or another controlled
HTTPS egress path.

Grant the controller service account a least-privilege role containing:

- `compute.instances.create`, `compute.instances.get`,
  `compute.instances.list`, and `compute.instances.delete`
- `compute.zoneOperations.get`
- `compute.images.useReadOnly` and `compute.disks.create`
- `compute.subnetworks.use`
- `compute.instances.setLabels`, `compute.instances.setMetadata`, and
  `compute.instances.setServiceAccount`
- `iam.serviceAccounts.actAs`, scoped only to the service account named by
  `GCP_RUNNER_SERVICE_ACCOUNT_EMAIL` (for example through
  `roles/iam.serviceAccountUser` on that service account)

Add `compute.subnetworks.useExternalIp` only when `GCP_EXTERNAL_IP=true`.

### Azure VMs

Install a public SSH key file separately; its private key need not exist on the
controller unless operators require SSH access:

```dotenv
COMPUTE_PROVIDER=azure
RUNNER_SPOT=true
AZURE_SUBSCRIPTION_ID=subscription-id
AZURE_RESOURCE_GROUP=github-runners
AZURE_LOCATION=westus2
AZURE_VM_SIZE=Standard_D4s_v5
AZURE_IMAGE=Canonical:ubuntu-24_04-lts:server:24.04.202601010
AZURE_SUBNET_ID=/subscriptions/.../subnets/runners
AZURE_ADMIN_USERNAME=runneradmin
AZURE_SSH_PUBLIC_KEY_FILE=/etc/github-runners/runner-admin.pub
```

Azure image versions must be pinned; `latest` is rejected. Spot defaults to a
maximum price of `-1` (the on-demand ceiling) and uses the `Delete` eviction
policy. Password authentication is disabled, and the OS disk and NIC are marked
for deletion. The controller also reconciles an orphaned controller-owned NIC after a Spot
eviction. Runner NICs receive no public IP from this controller, so provide NAT
or another controlled HTTPS egress path.

Grant the controller identity only these actions. VM, disk and NIC actions are
scoped to the configured resource group; the subnet join is scoped to the
selected subnet, which usually lives in a different resource group.

Scoped to the resource group:

- `Microsoft.Compute/virtualMachines/read`
- `Microsoft.Compute/virtualMachines/write`
- `Microsoft.Compute/virtualMachines/delete`
- `Microsoft.Compute/disks/read`
- `Microsoft.Compute/disks/write`
- `Microsoft.Compute/disks/delete`
- `Microsoft.Network/networkInterfaces/read`
- `Microsoft.Network/networkInterfaces/write`
- `Microsoft.Network/networkInterfaces/delete`
- `Microsoft.Network/networkInterfaces/join/action`

Scoped to the selected subnet:

- `Microsoft.Network/virtualNetworks/subnets/join/action`

The disk and NIC entries are needed even though the controller never calls those
APIs directly: the OS disk and NIC are created with `DeleteOption: Delete`, so
Azure creates and removes them on the controller's behalf as part of the VM
lifecycle.

### Vultr Cloud Compute

```dotenv
COMPUTE_PROVIDER=vultr
VULTR_API_KEY=read-from-your-secret-manager-at-deploy-time
VULTR_REGION=ewr
VULTR_PLAN=vc2-4c-8gb
# Ubuntu 24.04 LTS x64 OS ID (default 2284); omit when using VULTR_SNAPSHOT_ID.
VULTR_OS_ID=2284
VULTR_SNAPSHOT_ID=
VULTR_VPC_ID=private-vpc-id
VULTR_FIREWALL_GROUP_ID=deny-all-inbound-firewall-group-id
VULTR_SSH_KEY_IDS=
```

The Vultr adapter uses the Vultr API v2 directly over `net/http` with bounded
timeouts and response reads, base64-encoded cloud-init user data, controller and
job ownership tags, ambiguous-create recovery polling, authoritative 404
absence checks, and cursor-paginated orphan sweeps. Provision the private VPC
and deny-inbound firewall group with [`terraform/vultr`](terraform/vultr).
Powered-off Vultr instances continue billing until destroyed by the controller.

### Akamai Cloud / Linode

```dotenv
COMPUTE_PROVIDER=linode
LINODE_TOKEN=read-from-your-secret-manager-at-deploy-time
LINODE_REGION=us-east
LINODE_INSTANCE_TYPE=g6-standard-4
LINODE_IMAGE=linode/ubuntu24.04
LINODE_FIREWALL_ID=12345
LINODE_VPC_SUBNET_ID=
LINODE_SSH_PUBLIC_KEY_FILE=
```

The Linode adapter uses the Linode API v4 directly over `net/http` with bounded
timeouts and response reads, base64 cloud-init metadata (`metadata.user_data`),
cryptographically random throwaway root passwords (with root and password SSH
disabled by cloud-init), controller and job ownership tags, `X-Filter` tag
queries, ambiguous-create recovery polling, authoritative 404 absence checks,
and paginated orphan sweeps. Provision the VPC, subnet, and default-DROP Cloud
Firewall with [`terraform/linode`](terraform/linode). Powered-off Linode
instances continue billing until destroyed by the controller.

Drain all active records before changing `COMPUTE_PROVIDER`. State is bound to
the provider that created each runner, and the controller fails closed instead
of passing an old resource ID to a different provider adapter.

## Multi-cloud routing, spend caps, and fast-boot images

### Cross-cloud failover and cost-aware routing

Set `CLOUD_PROVIDERS` to a comma-separated list of two or more configured
providers (for example `vultr,linode,digitalocean,aws,gcp,azure`) to enable
`internal/router.MultiCloudRouter`:

```dotenv
CLOUD_PROVIDERS=vultr,linode,digitalocean
ROUTER_STRATEGY=cheapest
# Optional: override built-in USD/hour reference rates (provider:size=rate)
ROUTER_HOURLY_RATES=vultr:vc2-4c-8gb=0.071,linode:g6-standard-4=0.072
```

- `ROUTER_STRATEGY=priority` (default) tries backends in list order.
- `ROUTER_STRATEGY=cheapest` sorts backends ascending by hourly USD rate and
  breaks ties by configured order.
- Provisioning automatically fails over to the next backend on quota, capacity,
  or transient 5xx/429 errors, while refusing to fail over on ambiguous creates
  (`ErrCreateOutcomeUnknown`) to prevent duplicate VMs.
- Every instance ID is stored with its owning provider prefix (`<provider>:<id>`)
  so deletion, absence confirmation, and age-gated orphan sweeps always target
  the exact owning cloud.

### Hard spend circuit breaker

Configure daily, monthly, or per-repository USD caps to fail closed on new
provisioning without ever blocking deletion or orphan cleanup of running VMs:

```dotenv
BUDGET_DAILY_USD=25
BUDGET_MONTHLY_USD=400
BUDGET_REPO_DAILY_USD=10
BUDGET_STATE_FILE=/var/lib/github-runners/budget.json
```

`GET /metrics` exposes `budget_blocked_total` and
`estimated_spend_usd{window="daily|monthly|in_flight"}` in Prometheus text
format.

### JIT config, pre-baked images, and optional egress allowlist

```dotenv
# Single-use generate-jitconfig (default true); set false only for registration-token fallback.
GITHUB_USE_JIT_CONFIG=true
# Skip apt-get and runner tarball download when booting from a Packer image.
RUNNER_PREBAKED=false
# Optional comma-separated host/CIDR HTTPS allowlist (off by default).
RUNNER_EGRESS_ALLOWLIST=
```

Build pre-baked images across all six clouds using the HCL2 templates under
[`packer/`](packer/README.md).

## Build, test, and deploy

Deployment is gated on a staging run that uses the intended provider settings.
Before `make deploy`, verify provider permissions and quotas, private networking
and HTTPS egress, `MAX_LIVE_RUNNERS`, and ownership tags. Exercise successful
create/complete, cancellation, provider preemption, controller restart during
provision and deletion, and state backup/restore. Do not promote the build until
the provider console, GitHub runner inventory, and durable state agree after
each scenario.

```bash
make test
make build
make deploy
```

`make deploy` targets the SSH host alias `runner-host`, installs the binary,
cloud-init template, and hardened systemd unit, then starts the service. Create
the `webhook` system user and secret files first. The deployment disables the
obsolete standalone `cleanup.timer`; reconciliation runs inside the durable
controller.

Configure the GitHub App webhook URL as `https://your-domain.example/webhook`.
The reverse proxy must preserve the body and GitHub signature, delivery, event,
and installation metadata.

## Workflow usage

Every requested label must be included in `ALLOWED_LABELS`:

```yaml
jobs:
  integration:
    runs-on: [self-hosted, chef]
```

Keep ordinary lint and unit jobs on GitHub-hosted runners.

## Operations

- Forward controller and cloud audit logs to durable external storage.
- Alert on state records that reach `failed` or `orphaned`, and on elevated Spot
  preemption. Orphaned provider resources become eligible for the age-gated
  provider sweep rather than entering an unbounded deletion loop.
- Rotate cloud credentials and the GitHub App key according to policy.
- Test image and checksum upgrades in a dedicated private repository.
- Treat `MAX_RUNNER_AGE` as a fail-safe execution limit.
- Set `MAX_LIVE_RUNNERS` to the maximum billed fleet size; `WORKER_COUNT` only
  controls how many lifecycle operations the controller processes in parallel.
- DigitalOcean orphan records retain fleet reservations while their droplet IDs
  still exist. Each reconciliation checks those exact IDs and releases a
  reservation only after an authoritative 404, without waiting for
  `MAX_RUNNER_AGE`. API failures and existing resources retain their slots;
  GitHub identity cleanup and the original error history remain separate.
- Accepted DigitalOcean deletions are journaled against the exact droplet ID.
  The controller checks for an authoritative 404 before duplicate cleanup or
  releasing fleet capacity. While deletion propagates, it performs read-only
  confirmation rather than deleting again or treating disappearing tags as an
  ownership mismatch. Confirmation waits survive restarts and do not consume
  the deletion retry budget; API failures retain the fleet reservation.
- Cancelled jobs without an assigned runner are reclaimed after
  `CANCELLED_RUNNER_TTL` rather than the full maximum runner age.
- Run exactly one controller process against a state file; it contains lifecycle
  metadata and resource IDs but no credentials.
- State mutations are fsynced to `<STATE_FILE>.wal` in constant-size journal
  entries and periodically compacted into the atomic `STATE_FILE` snapshot.
- Deleted records are retained for webhook-redelivery deduplication and pruned
  after `DELETED_RECORD_RETENTION` (24 hours by default).

## Localization

The `webhook` and `installation-router` binaries embed translations for 48 locales in `internal/i18n`: 20 Tier 1 (`en`, `de`, `es`, `fr`, `it`, `pt-BR`, `nl`, `ru`, `uk`, `ja`, `ko`, `zh-CN`, `zh-TW`, `ar`, `hi`, `pl`, `tr`, `sv`, `vi`, `id`), 15 African (`af-ZA`, `am-ET`, `ar-EG`, `ar-MA`, `fr-MA`, `fr-SN`, `ha-NG`, `ig-NG`, `mg-MG`, `rw-RW`, `so-SO`, `sw-KE`, `xh-ZA`, `yo-NG`, `zu-ZA`), 10 Asian (`bn`, `fa`, `he`, `mr`, `ta`, `te`, `th`, `ur`, `ms`, `fil`), and 3 South Slavic (`bs`, `hr`, `sr`), with RTL support for `ar`, `ar-EG`, `ar-MA`, `fa`, `he`, and `ur`. Localized READMEs for all 47 non-English locales live under `docs/i18n/<locale>/README.md`.

Locale selection honors POSIX environment variables (`LC_ALL`, `LC_MESSAGES`, `LANG`) and can be overridden with `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

Unset, `C`, `POSIX`, and unrecognized locales default to English.

## Development

```bash
go test ./...
go run cmd/webhook/main.go
```

## License

MIT
