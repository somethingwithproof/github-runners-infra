> [English](../../../README.md)

# Multi-Cloud GitHub Actions JIT-Runner

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lizenz](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Ein Go-Controller, der ephemere GitHub Actions Runner für einzelne Jobs und auf Repository-Ebene auf DigitalOcean, AWS EC2, Google Compute Engine oder Azure-VMs erstellt.

## Sicherheitsarchitektur

- Überprüft Webhook-HMAC-Signaturen, Installations-IDs, Zustellungs-IDs und Repository-Allowlists.
- Speichert Zustellungen dauerhaft vor der Bestätigung und prüft vor der Bereitstellung über die GitHub Actions API, ob Jobs noch in der Warteschlange stehen.
- Erzeugt einmalig verwendbare GitHub-JIT-Runner-Konfigurationen auf Repository-Ebene und kennzeichnet jede VM vor der Erstellung mit der Controller-Zugehörigkeit.
- Löscht nach Abschluss des Jobs oder Ablauf der TTL ausschließlich vom Controller verwaltete Instanzen.

## Unterstützte Anbieter

| `COMPUTE_PROVIDER` | Standardkapazität | Unterbrechbare Kapazität |
| --- | --- | --- |
| `digitalocean` | Droplet | Nicht verfügbar (`RUNNER_SPOT=true` schlägt im Fail-Closed-Modus fehl) |
| `aws` | EC2 On-Demand | EC2 One-Time Spot |
| `gcp` | Standard-VM | Spot-VM |
| `azure` | Standard-VM | Spot-VM |

## Lokalisierung

Die Binärdateien `webhook` und `installation-router` betten Übersetzungen für 45 Locales (20 Tier-1-, 15 afrikanische und 10 asiatische Locales) in `internal/i18n` ein. Die Locale-Auswahl berücksichtigt `LC_ALL`, `LC_MESSAGES`, `LANG` und `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Entwicklung und Lizenz

```bash
go test ./...
make build
```

Details zu Bereitstellung und Betrieb finden sich in der vollständigen [englischen README](../../../README.md). Lizenziert unter [MIT](../../../LICENSE).
