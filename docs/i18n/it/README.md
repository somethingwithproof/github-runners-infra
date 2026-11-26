> [English](../../../README.md)

# Runner JIT GitHub Actions multi-cloud

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licenza](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Un controller Go che crea runner GitHub Actions effimeri, a singolo job e limitati al repository su DigitalOcean, AWS EC2, Google Compute Engine o VM Azure.

## Architettura di sicurezza

- Verifica le firme HMAC dei webhook, gli ID di installazione, gli ID di consegna e le allowlist dei repository.
- Salva in modo durevole le consegne prima della conferma e verifica tramite l'API GitHub Actions che i job siano ancora in coda prima del provisioning.
- Genera configurazioni runner JIT GitHub monouso limitate al repository ed etichetta ogni VM con la proprietà del controller prima della creazione.
- Elimina esclusivamente le istanze di proprietà del controller al termine del job o alla scadenza del TTL.

## Provider supportati

| `COMPUTE_PROVIDER` | Capacità standard | Capacità interrompibile |
| --- | --- | --- |
| `digitalocean` | Droplet | Non disponibile (`RUNNER_SPOT=true` fallisce in modalità fail-closed) |
| `aws` | EC2 On-Demand | EC2 Spot monouso |
| `gcp` | VM standard | VM Spot |
| `azure` | VM standard | VM Spot |

## Localizzazione

I binari `webhook` e `installation-router` integrano le traduzioni per 45 localizzazioni (20 Tier 1, 15 africane e 10 asiatiche) in `internal/i18n`. La selezione della lingua rispetta `LC_ALL`, `LC_MESSAGES`, `LANG` e `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Sviluppo e licenza

```bash
go test ./...
make build
```

Consulta il [README completo in inglese](../../../README.md) per i dettagli su deployment e operatività. Rilasciato sotto licenza [MIT](../../../LICENSE).
