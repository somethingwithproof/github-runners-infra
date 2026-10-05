# Mantl ephemeral runner pool

Deploy a dedicated controller beside the legacy somethingwithproof and Kadupul
controllers. Its GitHub App installation is somethingwithproof's existing
installation, but repository-scoped tokens and both repository allowlists contain
only `somethingwithproof/mantl`. Its required label is `mantl-do`.

The installation router's optional `MANTL_RUNNERS_ENABLED=true` selects this
controller only when both the legacy installation ID and Mantl's exact repository
identity match. All other repositories keep their existing routes. Every selected
controller independently verifies the original signed delivery and GitHub's run
origin before allocating capacity. Router selection grants no provisioning access.

Use a dedicated sfo3 VPC and a firewall targeting `runner-controller-mantl` with
no inbound rules and outbound HTTP/HTTPS/DNS. Start with four live
`s-8vcpu-16gb` runners and four workers; there is no idle workload capacity.
The existing controller host remains running. State lives in
`/var/lib/github-runners-mantl`; the controller listens only on 127.0.0.1:8083.

Install the tested controller binary as `/usr/local/bin/mantl-webhook`, the
reviewed template as `/opt/github-runners-mantl/cloud-init/runner-mantl.yaml.tmpl`,
and `webhook.service` as `mantl-webhook.service`. Supply configuration in
`/etc/github-runners/mantl-env` with mode 0600 and the existing webhook service
owner. Copy secrets only from protected runtime configuration on the controller;
never transfer or commit their values. The App key remains on the controller.

The template omits Chef and preinstalled application runtimes. Mantl's shared mise
action selects each job's pinned tools. Docker, buildx, gh, libraries and the
writable hosted tool cache provide the Linux job prerequisites. Startup failures
power off the VM; controller TTL cleanup must still delete it to end billing.
Runner artifacts are checksum verified against reviewed official releases.

Enable the repository's routing variable only after its smoke run, runtime setup,
container execution and actual VM deletion succeed. Then validate ordinary CI on
the exact migration PR head before merging. See Mantl's
`docs/digitalocean-runners.md` for event eligibility and rollback.

Rollback first disables `DO_RUNNERS_ENABLED` and cancels queued self-hosted runs.
Drain active work, verify owned VM cleanup, and disable the router's Mantl override
before stopping the new service. Keep tested binary/unit backups on the controller
host; do not change or stop the existing legacy or Kadupul services.
