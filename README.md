# Multi-cloud GitHub Actions JIT Runners

A Go controller that creates one-job, repository-scoped GitHub Actions runners
on DigitalOcean, AWS EC2, Google Compute Engine, or Azure VMs. GitHub webhook
deliveries are persisted before acknowledgement, provisioning is retried after
transient failures or controller restarts, and only controller-owned resources
can be deleted.

## Security architecture

```text
GitHub workflow_job webhook
  -> verify HMAC, installation ID, delivery ID, private-repository allowlist
  -> durably record the job
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

The controller rejects public repositories. Self-hosted workflows still execute
repository code with root-equivalent Docker access on an isolated VM, so permit
only explicitly trusted private repositories. Prefer private subnets with
egress through NAT and security groups or firewall rules with no inbound access.

## Supported providers

| `COMPUTE_PROVIDER` | Standard capacity | Interruptible capacity |
| --- | --- | --- |
| `digitalocean` | Droplet | Not available; `RUNNER_SPOT=true` fails closed |
| `aws` | EC2 On-Demand | EC2 one-time Spot, terminate on interruption |
| `gcp` | Standard VM | Spot VM, delete on preemption |
| `azure` | Standard VM | Spot VM, delete eviction policy |

`RUNNER_SPOT` defaults to `false`. Spot instances can disappear at any time and
have no availability guarantee. A preempted GitHub job may need to be retried by
workflow policy; the lifecycle TTL remains the cleanup backstop.

DigitalOcean firewall posture is validated through its API before provisioning.
AWS security-group, GCP VPC-firewall, and Azure subnet/NSG ingress policy is
explicitly delegated to operator-managed IaC because the intended policy cannot
be inferred from a subnet identifier. Keep those runners private and deny all
unsolicited inbound traffic; enabling an external IP requires a separate policy
review.

## Prerequisites

- Go 1.25.13 or newer
- A Linux controller host with Caddy or another TLS reverse proxy
- A GitHub App subscribed to `workflow_job` with repository Administration: write
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
ALLOWED_LABELS=self-hosted,chef
REQUIRED_LABEL=self-hosted

RUNNER_VERSION=2.336.0
RUNNER_SHA256=04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d
CHEF_INSTALLER_SHA256=replace-with-the-reviewed-installer-digest

RUNNER_GROUP_ID=1
WORKER_COUNT=4
MAX_LIVE_RUNNERS=20
MAX_ATTEMPTS=5
MAX_RUNNER_AGE=6h
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

All allowlisted repositories must belong to the same GitHub App installation
owner. Installation tokens are restricted to exactly those repository names and
only the `administration: write` permission needed for runner management.

Do not derive an integrity value and execute the artifact in the same automated
step. Obtain runner digests from official release instructions and review the
Chef installer digest during an intentional upgrade.

### DigitalOcean

```dotenv
COMPUTE_PROVIDER=digitalocean
DIGITALOCEAN_TOKEN=read-from-your-secret-manager-at-deploy-time
DO_REGION=nyc3
DO_SIZE=s-4vcpu-8gb
DO_IMAGE=ubuntu-24-04-x64
DO_VPC_UUID=private-vpc-uuid
DO_FIREWALL_ID=deny-all-inbound-firewall-id
```

Place runners in a dedicated VPC and configure the required Cloud Firewall with
no inbound rules. It must target the `runner-controller-${CONTROLLER_ID}` tag so
it applies as each tagged Droplet is created; the controller verifies both the
rules and tag before every create. Runner cloud-init also disables root and
password SSH access. For a custom-scope token, grant `droplet:create`,
`droplet:read`, `droplet:delete`, `tag:create`, `tag:read`, `vpc:read`, and
`firewall:read`. DigitalOcean also requires `regions:read`, `sizes:read`,
`actions:read`, and `image:read` with `droplet:create`. The controller only
reads the preconfigured Firewall; it does not need Firewall write scopes. Do
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

Grant the controller identity only the VM and network-interface read/write/delete
permissions required in the configured resource group and permission to join the
selected subnet.

Drain all active records before changing `COMPUTE_PROVIDER`. State is bound to
the provider that created each runner, and the controller fails closed instead
of passing an old resource ID to a different provider adapter.

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
- Cancelled jobs without an assigned runner are reclaimed after
  `CANCELLED_RUNNER_TTL` rather than the full maximum runner age.
- Run exactly one controller process against a state file; it contains lifecycle
  metadata and resource IDs but no credentials.
- State mutations are fsynced to `<STATE_FILE>.wal` in constant-size journal
  entries and periodically compacted into the atomic `STATE_FILE` snapshot.
- Deleted records are retained for webhook-redelivery deduplication and pruned
  after `DELETED_RECORD_RETENTION` (24 hours by default).

## Development

```bash
go test ./...
go run cmd/webhook/main.go
```

## License

MIT
