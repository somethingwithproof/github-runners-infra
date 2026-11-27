# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

output "vcn_id" {
  description = "Dedicated private runner VCN OCID."
  value       = oci_core_vcn.runners.id
}

output "subnet_id" {
  description = "Value for OCI_SUBNET_OCID."
  value       = oci_core_subnet.runners.id
}

output "security_list_id" {
  description = "Egress-only security list OCID attached to the runner subnet."
  value       = oci_core_security_list.runners.id
}

output "controller_dynamic_group_id" {
  description = "Least-privilege OCI IAM Dynamic Group OCID for the webhook controller."
  value       = oci_identity_dynamic_group.controller.id
}
