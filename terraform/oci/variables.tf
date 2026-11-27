# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

variable "compartment_ocid" {
  description = "Dedicated OCI compartment OCID for ephemeral runner compute and network resources (OCI_COMPARTMENT_OCID)."
  type        = string

  validation {
    condition     = can(regex("^ocid1\\.(compartment|tenancy)\\.", var.compartment_ocid))
    error_message = "compartment_ocid must be a valid OCI compartment or tenancy OCID."
  }
}

variable "controller_id" {
  description = "Unique controller ownership identifier (CONTROLLER_ID)."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", var.controller_id))
    error_message = "controller_id must start with an alphanumeric character and contain only letters, numbers, underscores, and hyphens (max 64 chars)."
  }
}

variable "controller_group_ocid" {
  description = "OCID of the IAM group containing the webhook API signing user."
  type        = string

  validation {
    condition     = can(regex("^ocid1\\.group\\.[a-zA-Z0-9.-]+$", var.controller_group_ocid))
    error_message = "controller_group_ocid must be a valid OCI group OCID."
  }
}

variable "vcn_cidr" {
  description = "Private RFC1918 CIDR block for the dedicated runner VCN."
  type        = string
  default     = "10.90.0.0/16"
}

variable "subnet_cidr" {
  description = "Private subnet CIDR inside vcn_cidr for ephemeral runner instances."
  type        = string
  default     = "10.90.1.0/24"
}

variable "freeform_tags" {
  description = "Additional freeform tags applied to all created OCI resources."
  type        = map(string)
  default     = {}
}
