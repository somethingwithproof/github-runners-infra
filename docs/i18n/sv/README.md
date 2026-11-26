> [English](../../../README.md)

# JIT-runners för GitHub Actions i flera moln

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licens](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

En Go-kontroller som skapar efemära GitHub Actions-runners för enskilda jobb, begränsade till ett arkiv, på virtuella maskiner hos DigitalOcean, AWS EC2, Google Compute Engine eller Azure.

## Säkerhetsarkitektur

- Verifierar HMAC-signaturer för webhooks, installations-ID:n, leverans-ID:n och tillåtelselistor för arkiv.
- Lagrar leveranser beständigt före bekräftelse och verifierar via GitHub Actions API att jobb fortfarande står i kö före provisionering.
- Genererar engångskonfigurationer för GitHub JIT-runners begränsade till ett arkiv och taggar varje VM med kontrollerägarskap innan den skapas.
- Tar endast bort instanser som ägs av kontrollern efter avslutat jobb eller när TTL löper ut.

## Leverantörer som stöds

| `COMPUTE_PROVIDER` | Standardkapacitet | Avbrytbar kapacitet |
| --- | --- | --- |
| `digitalocean` | Droplet | Inte tillgänglig (`RUNNER_SPOT=true` misslyckas säkert) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Lokalisering

Binärfilerna `webhook` och `installation-router` bäddar in översättningar för 45 språkområden (20 Tier 1-, 15 afrikanska och 10 asiatiska språkområden) i `internal/i18n`. Språkvalet följer `LC_ALL`, `LC_MESSAGES`, `LANG` och `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Utveckling och licens

```bash
go test ./...
make build
```

Se den fullständiga [engelska README-filen](../../../README.md) för information om driftsättning och drift. Licensierad under [MIT](../../../LICENSE).
