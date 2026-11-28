# Terraform Modules for Multi-Cloud GitHub Actions Runners

Each subdirectory provisions the least-privilege networking, firewall, and IAM
prerequisites for one supported compute provider:

- [`digitalocean/`](./digitalocean): private VPC, controller tag, and deny-all-inbound Cloud Firewall
- [`aws/`](./aws): egress-only security group and least-privilege EC2 controller IAM role
- [`gcp/`](./gcp): zero-role runner service account, optional binding to an operator-supplied controller role, and VPC firewall rules
- [`azure/`](./azure): runner resource group, deny-inbound NSG, and scoped custom role definition
- [`vultr/`](./vultr): private VPC and deny-all-inbound Firewall Group
- [`linode/`](./linode): private VPC, subnet, and default-DROP Cloud Firewall (HTTPS and DNS egress only)

Validate formatting locally with:

```bash
terraform fmt -check -recursive terraform/
```
