# GCP ephemeral GitHub Actions runners

Terraform for a dedicated GCP project (`swp-ci-runners`) that hosts Spot,
single-job, self-deleting GitHub Actions runner VMs. A small webhook host
creates and tears down runner VMs in response to GitHub job events.

## Layout

- `versions.tf`   provider/backend pinning
- `project.tf`    project + enabled APIs
- `iam.tf`        the two service accounts and their (least-privilege) bindings
- `network.tf`    VPC, subnet, Cloud Router + NAT, firewall
- `secrets.tf`    four Secret Manager secret shells (no values)
- `compute.tf`    Spot runner instance template + webhook host VM
- `variables.tf`  all knobs
- `outputs.tf`    project id, SA emails, network/subnet links, secret ids

## Prerequisites

- A billing account you can attach (default `01B4AB-92453F-05CB01`).
- Set exactly one of `org_id` or `folder_id` (the project's parent).
- The caller's identity needs project-creation rights on that parent plus
  billing-account user on the billing account.
- The S3 state backend (`picasso-terraform-state`) exists. For local checks
  use `-backend=false` (see below).

## Apply order

Terraform resolves most of this through `depends_on`, but the intended order is:

1. `google_project.runners` — the project.
2. `google_project_service.enabled` — compute, iam, secretmanager, logging.
   Everything else depends on these.
3. Service accounts (`runner-control`, `runner-node`) and their IAM bindings.
4. Network: VPC → subnet → router → NAT → firewall.
5. Secrets (shells only).
6. Compute: runner instance template, then the webhook host.

Run:

```sh
terraform init -backend-config=...        # supply real backend settings
terraform plan
terraform apply
```

## Secret values (manual, never committed)

The four secrets are created empty. Add versions out-of-band:

```sh
PROJECT=swp-ci-runners
printf '%s' "$GITHUB_APP_ID"          | gcloud secrets versions add github-app-id          --project="$PROJECT" --data-file=-
printf '%s' "$GITHUB_INSTALLATION_ID" | gcloud secrets versions add github-installation-id --project="$PROJECT" --data-file=-
gcloud secrets versions add github-app-private-key --project="$PROJECT" --data-file=./app-private-key.pem
printf '%s' "$GITHUB_WEBHOOK_SECRET"  | gcloud secrets versions add github-webhook-secret  --project="$PROJECT" --data-file=-
```

Never put secret values in `.tfvars`, the state backend payload review, or git.
Only the `runner-control` SA can read these secrets (per-secret
`secretAccessor`, not project-wide).

## Networking and ingress

Runner VMs have no external IP and reach GitHub through Cloud NAT. There is no
inbound firewall allow rule; an explicit `deny-all-ingress` rule documents the
fail-closed posture. GitHub webhook delivery reaches the webhook host over a
**cloudflared tunnel provisioned separately** (an outbound connection from the
host to Cloudflare's edge); nothing is opened inbound here.

## Local validation

```sh
terraform fmt -recursive .
terraform init -backend=false
terraform validate
```
