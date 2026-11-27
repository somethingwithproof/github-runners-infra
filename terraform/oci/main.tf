# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

locals {
  common_tags = merge(var.freeform_tags, {
    "github-runners-controller" = var.controller_id
    "ManagedBy"                 = "terraform"
  })
  dg_name = "github-runners-controller-${var.controller_id}"
  matching_rule = (
    var.controller_instance_ocid != ""
    ? "instance.id = '${var.controller_instance_ocid}'"
    : "ALL {instance.compartment.id = '${var.compartment_ocid}', tag.github-runners-controller = '${var.controller_id}'}"
  )
}

resource "oci_core_vcn" "runners" {
  compartment_id = var.compartment_ocid
  cidr_blocks    = [var.vcn_cidr]
  display_name   = "github-runners-${var.controller_id}"
  dns_label      = "ghrunners"
  freeform_tags  = local.common_tags
}

resource "oci_core_nat_gateway" "runners" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.runners.id
  display_name   = "github-runners-${var.controller_id}-nat"
  freeform_tags  = local.common_tags
}

resource "oci_core_route_table" "runners" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.runners.id
  display_name   = "github-runners-${var.controller_id}-private"
  freeform_tags  = local.common_tags

  route_rules {
    description       = "Private subnet egress via NAT gateway"
    destination       = "0.0.0.0/0"
    destination_type  = "CIDR_BLOCK"
    network_entity_id = oci_core_nat_gateway.runners.id
  }
}

# Egress-only security list: zero ingress rules, HTTPS and DNS egress only.
resource "oci_core_security_list" "runners" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.runners.id
  display_name   = "github-runners-${var.controller_id}-egress-only"
  freeform_tags  = local.common_tags

  egress_security_rules {
    description      = "HTTPS egress"
    destination      = "0.0.0.0/0"
    destination_type = "CIDR_BLOCK"
    protocol         = "6"
    stateless        = false

    tcp_options {
      min = 443
      max = 443
    }
  }

  egress_security_rules {
    description      = "DNS UDP egress"
    destination      = "0.0.0.0/0"
    destination_type = "CIDR_BLOCK"
    protocol         = "17"
    stateless        = false

    udp_options {
      min = 53
      max = 53
    }
  }

  egress_security_rules {
    description      = "DNS TCP egress"
    destination      = "0.0.0.0/0"
    destination_type = "CIDR_BLOCK"
    protocol         = "6"
    stateless        = false

    tcp_options {
      min = 53
      max = 53
    }
  }
}

resource "oci_core_subnet" "runners" {
  compartment_id             = var.compartment_ocid
  vcn_id                     = oci_core_vcn.runners.id
  cidr_block                 = var.subnet_cidr
  display_name               = "github-runners-${var.controller_id}-private"
  dns_label                  = "runners"
  prohibit_public_ip_on_vnic = true
  route_table_id             = oci_core_route_table.runners.id
  security_list_ids          = [oci_core_security_list.runners.id]
  freeform_tags              = local.common_tags
}

resource "oci_identity_dynamic_group" "controller" {
  compartment_id = var.tenancy_ocid
  name           = local.dg_name
  description    = "Least-privilege dynamic group for GitHub Actions runner controller ${var.controller_id}"
  matching_rule  = local.matching_rule
  freeform_tags  = local.common_tags
}

resource "oci_identity_policy" "controller" {
  compartment_id = var.compartment_ocid
  name           = "github-runners-controller-${var.controller_id}"
  description    = "Least-privilege compute and subnet join policy scoped to the runner compartment"
  freeform_tags  = local.common_tags

  statements = [
    "Allow dynamic-group ${oci_identity_dynamic_group.controller.name} to manage instances in compartment id ${var.compartment_ocid}",
    "Allow dynamic-group ${oci_identity_dynamic_group.controller.name} to use subnets in compartment id ${var.compartment_ocid}",
    "Allow dynamic-group ${oci_identity_dynamic_group.controller.name} to use vnics in compartment id ${var.compartment_ocid}",
    "Allow dynamic-group ${oci_identity_dynamic_group.controller.name} to use vnic-attachments in compartment id ${var.compartment_ocid}",
    "Allow dynamic-group ${oci_identity_dynamic_group.controller.name} to read instance-images in compartment id ${var.compartment_ocid}",
  ]
}
