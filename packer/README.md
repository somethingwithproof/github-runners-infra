# Pre-baked Runner Images (Packer HCL2)

These Packer templates build hardened Ubuntu 24.04 runner images with Docker,
build tools, and a SHA-256-verified `actions/runner` archive pre-installed in
`/home/runner/actions-runner`. Booting from a pre-baked image with
`RUNNER_PREBAKED=true` skips `apt-get` and tarball downloads during cloud-init.

## Templates

| Cloud | Template | Credential source |
| --- | --- | --- |
| DigitalOcean | [`digitalocean.pkr.hcl`](./digitalocean.pkr.hcl) | `DIGITALOCEAN_TOKEN` |
| AWS | [`aws.pkr.hcl`](./aws.pkr.hcl) | AWS SDK default credential chain |
| GCP | [`gcp.pkr.hcl`](./gcp.pkr.hcl) | Application Default Credentials |
| Azure | [`azure.pkr.hcl`](./azure.pkr.hcl) | `ARM_SUBSCRIPTION_ID` + Azure CLI / Managed Identity |
| Vultr | [`vultr.pkr.hcl`](./vultr.pkr.hcl) | `VULTR_API_KEY` |
| Linode / Akamai | [`linode.pkr.hcl`](./linode.pkr.hcl) | `LINODE_TOKEN` |

## Build example

```bash
packer init packer/vultr.pkr.hcl
packer validate \
  -var "runner_version=2.337.0" \
  -var "runner_sha256=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613" \
  packer/vultr.pkr.hcl
packer build \
  -var "runner_version=2.337.0" \
  -var "runner_sha256=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613" \
  packer/vultr.pkr.hcl
```
