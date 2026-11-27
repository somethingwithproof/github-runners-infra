# Runners Infra for GitHub Actions

<img src="docs/images/runners-infra.jpg" alt="Runners Infra" width="420">

A **runner** is the machine that carries out a GitHub Actions job, such as building
your app or running its tests. Runners Infra starts a fresh virtual machine in
your cloud account when a job needs one. The machine runs one job, then the
service deletes it.

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Quality Gate](https://sonarcloud.io/api/project_badges/measure?project=somethingwithproof_github-runners-infra&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=somethingwithproof_github-runners-infra)
[![Go minimum](https://img.shields.io/badge/Go_minimum-1.26.9-blue)](./go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/somethingwithproof/github-runners-infra/badge)](https://scorecard.dev/viewer/?uri=github.com/somethingwithproof/github-runners-infra)

## Why use it?

- **Reach services inside your network.** A test can reach a private database if
  you put the runner inside the same network and give it access. Other jobs in
  the same workflow can still run on GitHub-hosted machines.
- **Get a clean machine for each job.** Each job starts from the machine image you
  chose, so it won’t pick up files or containers left behind by an earlier job.
  The included setup installs Docker and Chef Workstation on each machine.
- **Keep fewer machines sitting idle.** If tests run in bursts, you can set a
  limit on how many runners are live at once and let the service remove them
  when the work finishes. You still pay for the controller and any cloud resources
  you keep running.

## How it works

You run a small service called the *controller* on a Linux host. Each
controller uses one provider. You can choose DigitalOcean, AWS EC2, Google
Compute Engine, or Azure.

1. GitHub tells the controller that a job is waiting. The controller checks the
   repository and the labels in the job’s `runs-on` setting against the ones you allow.
2. If the job is still waiting, the controller asks GitHub for a single-use
   registration and starts a machine with it. GitHub assigns it a matching job from that repository.
3. When the job finishes, the machine powers off and the controller removes it.
   The controller saves its progress so it can resume cleanup after a restart.

## Is it a good fit?

Use it when you need to choose where your jobs run, which machine size they get,
or which private services they can reach. For a lint check that already works on
a GitHub-hosted runner, you can keep using that runner and skip the cloud setup.

A fresh machine takes time to start. You also maintain the controller, its
credentials, and the runner network. Run only code you trust. A workflow with
Docker access can take control of its runner and reach services available to it.

## Get started

You’ll need a Linux host for the controller, a GitHub App installed on the
repositories you choose, and a cloud account where the controller can create and
delete machines. Follow the [setup guide](docs/runner-setup.md#prerequisites) to
configure the app, choose a provider, and deploy the controller.

Once the controller is running, use its labels in a workflow. With the example
configuration, this job checks that Docker is available on a fresh runner.

```yaml
name: Check runner
on: workflow_dispatch

jobs:
  check:
    runs-on: [self-hosted, chef]
    steps:
      - run: docker info
```

For permissions and network settings, see the [provider reference](docs/runner-setup.md#supported-providers).
For cleanup, runner limits, and monitoring, see [operations](docs/runner-setup.md#operations).

## Languages

**English** · [Deutsch](docs/i18n/de/README.md) · [Español](docs/i18n/es/README.md) · [Français](docs/i18n/fr/README.md) · [Português (BR)](docs/i18n/pt-BR/README.md) · [日本語](docs/i18n/ja/README.md) · [한국어](docs/i18n/ko/README.md) · [简体中文](docs/i18n/zh-CN/README.md) · [العربية](docs/i18n/ar/README.md) · [हिन्दी](docs/i18n/hi/README.md)

<details>
<summary>All 48 languages</summary>

**Tier 1:** **English** · [Deutsch](docs/i18n/de/README.md) · [Español](docs/i18n/es/README.md) · [Français](docs/i18n/fr/README.md) · [Italiano](docs/i18n/it/README.md) · [Português (BR)](docs/i18n/pt-BR/README.md) · [Nederlands](docs/i18n/nl/README.md) · [Русский](docs/i18n/ru/README.md) · [Українська](docs/i18n/uk/README.md) · [日本語](docs/i18n/ja/README.md) · [한국어](docs/i18n/ko/README.md) · [简体中文](docs/i18n/zh-CN/README.md) · [繁體中文](docs/i18n/zh-TW/README.md) · [العربية](docs/i18n/ar/README.md) · [हिन्दी](docs/i18n/hi/README.md) · [Polski](docs/i18n/pl/README.md) · [Türkçe](docs/i18n/tr/README.md) · [Svenska](docs/i18n/sv/README.md) · [Tiếng Việt](docs/i18n/vi/README.md) · [Bahasa Indonesia](docs/i18n/id/README.md)

**African:** [Afrikaans (ZA)](docs/i18n/af-ZA/README.md) · [አማርኛ (ET)](docs/i18n/am-ET/README.md) · [العربية (مصر)](docs/i18n/ar-EG/README.md) · [العربية (المغرب)](docs/i18n/ar-MA/README.md) · [Français (MA)](docs/i18n/fr-MA/README.md) · [Français (SN)](docs/i18n/fr-SN/README.md) · [Hausa (NG)](docs/i18n/ha-NG/README.md) · [Igbo (NG)](docs/i18n/ig-NG/README.md) · [Malagasy (MG)](docs/i18n/mg-MG/README.md) · [Kinyarwanda (RW)](docs/i18n/rw-RW/README.md) · [Soomaali (SO)](docs/i18n/so-SO/README.md) · [Kiswahili (KE)](docs/i18n/sw-KE/README.md) · [isiXhosa (ZA)](docs/i18n/xh-ZA/README.md) · [Yorùbá (NG)](docs/i18n/yo-NG/README.md) · [isiZulu (ZA)](docs/i18n/zu-ZA/README.md)

**Asian:** [বাংলা](docs/i18n/bn/README.md) · [فارسی](docs/i18n/fa/README.md) · [עברית](docs/i18n/he/README.md) · [मराठी](docs/i18n/mr/README.md) · [தமிழ்](docs/i18n/ta/README.md) · [తెలుగు](docs/i18n/te/README.md) · [ไทย](docs/i18n/th/README.md) · [اردو](docs/i18n/ur/README.md) · [Bahasa Melayu](docs/i18n/ms/README.md) · [Filipino](docs/i18n/fil/README.md)

**South Slavic:** [Bosanski](docs/i18n/bs/README.md) · [Hrvatski](docs/i18n/hr/README.md) · [Српски](docs/i18n/sr/README.md)

</details>

## Development

```bash
go test ./...
go run cmd/webhook/main.go
```

## License

MIT
