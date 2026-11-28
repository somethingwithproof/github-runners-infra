#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

set -euo pipefail

: "${RUNNER_VERSION:?RUNNER_VERSION must be set}"
: "${RUNNER_SHA256:?RUNNER_SHA256 must be set}"

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
  build-essential \
  ca-certificates \
  curl \
  docker.io \
  git \
  iptables \
  jq \
  libssl-dev \
  libyaml-dev \
  unzip \
  zlib1g-dev

systemctl enable docker

if ! id -u runner >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash --groups docker runner
fi

install -d -o runner -g runner /opt/hostedtoolcache
install -d -o runner -g runner /home/runner/actions-runner

tarball="/tmp/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz"
release_url="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz"

curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 \
  --output "${tarball}" \
  "${release_url}"

printf '%s  %s\n' "${RUNNER_SHA256}" "${tarball}" | sha256sum --check --strict -
tar --extract --gzip --file "${tarball}" --directory /home/runner/actions-runner
shred --remove "${tarball}"
chown -R runner:runner /home/runner/actions-runner

# Harden SSH and clean per-instance state so cloud-init runs cleanly on first boot.
install -d -m 0755 /etc/ssh/sshd_config.d
cat <<'EOF' > /etc/ssh/sshd_config.d/99-runner-hardening.conf
PermitRootLogin no
PasswordAuthentication no
EOF

apt-get clean
rm -rf /var/lib/apt/lists/*
cloud-init clean --logs || true
truncate -s 0 /etc/machine-id
