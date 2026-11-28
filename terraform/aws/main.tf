# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.70"
    }
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

variable "vpc_id" {
  description = "Existing VPC ID containing the private runner subnet."
  type        = string
}

variable "controller_principal_arn" {
  description = "IAM principal ARN allowed to assume the runner controller role."
  type        = string
}

variable "tags" {
  description = "Additional tags applied to all created AWS resources."
  type        = map(string)
  default     = {}
}

data "aws_partition" "current" {}
data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

locals {
  ec2_arn         = "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}"
  account_ec2_arn = "${local.ec2_arn}:${data.aws_caller_identity.current.account_id}"
  role_name       = "github-runners-controller-${var.controller_id}"
  common_tags = merge(var.tags, {
    "github-runners/controller" = var.controller_id
    "ManagedBy"                 = "terraform"
  })
}

resource "aws_security_group" "runners" {
  name        = "github-runners-${var.controller_id}"
  description = "Deny all inbound traffic; permit HTTPS and DNS egress for ephemeral runners"
  vpc_id      = var.vpc_id

  egress {
    description = "HTTPS egress"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "DNS UDP egress"
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "DNS TCP egress"
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.common_tags
}

data "aws_iam_policy_document" "controller_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "AWS"
      identifiers = [var.controller_principal_arn]
    }
  }
}

data "aws_iam_policy_document" "controller" {
  statement {
    sid       = "DescribeInstances"
    actions   = ["ec2:DescribeInstances"]
    resources = ["*"]
  }

  statement {
    sid       = "RunTaggedInstances"
    actions   = ["ec2:RunInstances"]
    resources = ["${local.account_ec2_arn}:instance/*"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/github-runners/controller"
      values   = [var.controller_id]
    }
  }

  statement {
    sid     = "UseLaunchResources"
    actions = ["ec2:RunInstances"]
    resources = [
      "${local.ec2_arn}::image/*",
      "${local.account_ec2_arn}:volume/*",
      "${local.account_ec2_arn}:subnet/*",
      "${local.account_ec2_arn}:network-interface/*",
      "${local.account_ec2_arn}:key-pair/*",
      aws_security_group.runners.arn,
    ]
  }

  statement {
    sid       = "TagNewInstances"
    actions   = ["ec2:CreateTags"]
    resources = ["${local.account_ec2_arn}:instance/*"]

    condition {
      test     = "StringEquals"
      variable = "ec2:CreateAction"
      values   = ["RunInstances"]
    }
  }

  statement {
    sid       = "TerminateOwnedInstances"
    actions   = ["ec2:TerminateInstances"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/github-runners/controller"
      values   = [var.controller_id]
    }
  }
}

resource "aws_iam_role" "controller" {
  name               = length(local.role_name) <= 64 ? local.role_name : "${substr(local.role_name, 0, 51)}-${substr(sha256(var.controller_id), 0, 12)}"
  assume_role_policy = data.aws_iam_policy_document.controller_assume.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy" "controller" {
  name   = "ec2-runner-lifecycle"
  role   = aws_iam_role.controller.id
  policy = data.aws_iam_policy_document.controller.json
}

output "security_group_id" {
  description = "Value for AWS_SECURITY_GROUP_IDS."
  value       = aws_security_group.runners.id
}

output "controller_role_arn" {
  description = "Least-privilege IAM role ARN for the webhook controller."
  value       = aws_iam_role.controller.arn
}
