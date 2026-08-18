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

## Prerequisites

- Go 1.25.13 or newer
- A Linux controller host with Caddy or another TLS reverse proxy
- A GitHub App subscribed to `workflow_job` with repository Administration: write
- Least-privilege credentials for exactly one supported cloud provider
- A reviewed Chef installer SHA-256 and GitHub runner archive SHA-256
- Outbound HTTPS from runner subnets, directly or through NAT

AWS uses the Go SDK default credential chain, GCP uses Application Default
Credentials, and Azure uses `DefaultAzureCredential`. Prefer workload identity
or a controller instance role over long-lived static credentials. The
DigitalOcean adapter reads `DIGITALOCEAN_TOKEN` because DigitalOcean's API client
does not provide an equivalent workload-identity chain.

## Common configuration

Create the GitHub App private-key file separately:

```bash
sudo install -o webhook -g webhook -m 0600 github-app.pem /etc/github-runners/github-app.pem
```

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

RUNNER_VERSION=2.331.0
RUNNER_SHA256=replace-with-the-reviewed-linux-x64-release-digest
CHEF_INSTALLER_SHA256=replace-with-the-reviewed-installer-digest

RUNNER_GROUP_ID=1
WORKER_COUNT=4
MAX_LIVE_RUNNERS=20
MAX_ATTEMPTS=5
MAX_RUNNER_AGE=6h
CANCELLED_RUNNER_TTL=5m
STATE_FILE=/var/lib/github-runners/state.json
DELETED_RECORD_RETENTION=24h
CLOUD_INIT_PATH=/opt/github-runners/cloud-init/runner.yaml.tmpl
LISTEN_ADDR=:8080
```

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
password SSH access. Grant only the Droplet, Tag, VPC, and Firewall read
operations needed to create, inspect, protect, and delete owned runners.

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
GCP_EXTERNAL_IP=false
```

The adapter uses deterministic names, ownership labels, idempotent request IDs,
auto-deleted boot disks, and `DELETE` as the Spot termination action. No service
account is attached by this controller. With `GCP_EXTERNAL_IP=false`, the subnet
must provide Cloud NAT or another controlled HTTPS egress path.

Grant the controller service account only the Compute permissions needed to get,
create, and delete instances and use the selected subnet and image.

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
for deletion. The controller also reconciles an orphaned owned NIC after a Spot
eviction. Runner NICs receive no public IP from this controller, so provide NAT
or another controlled HTTPS egress path.

Grant the controller identity only the VM and network-interface read/write/delete
permissions required in the configured resource group and permission to join the
selected subnet.

Drain all active records before changing `COMPUTE_PROVIDER`. State is bound to
the provider that created each runner, and the controller fails closed instead
of passing an old resource ID to a different provider adapter.

## Build, test, and deploy

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
  preemption. Orphaned records require operator cleanup but do not block fleet
  admission or enter an unbounded deletion loop.
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

## License

MIT
