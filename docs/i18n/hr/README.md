> [English](../../../README.md)

# Višeoblačni JIT izvršitelji za GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licencija](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Go upravljač koji izrađuje jednokratne, prolazne GitHub Actions izvršitelje ograničene na repozitorij na virtualnim računalima u okruženjima DigitalOcean, AWS EC2, Google Compute Engine ili Azure.

## Sigurnosna arhitektura

- Provjerava HMAC potpise webhook zahtjeva, identifikatore instalacija, identifikatore isporuka i popise dopuštenih repozitorija.
- Trajno pohranjuje zaprimljene događaje prije slanja potvrde te putem GitHub Actions API-ja provjerava čekaju li poslovi još u redu prije dodjele resursa.
- Izrađuje jednokratne GitHub JIT postavke ograničene na repozitorij i označava svako virtualno računalo oznakom vlasništva upravljača prije izrade.
- Briše isključivo instance u vlasništvu upravljača nakon završetka posla ili isteka TTL roka.

## Podržani pružatelji

| `COMPUTE_PROVIDER` | Standardni kapacitet | Prekidivi kapacitet |
| --- | --- | --- |
| `digitalocean` | Droplet | Nije dostupno (`RUNNER_SPOT=true` sigurno odbija pokretanje) |
| `aws` | EC2 On-Demand | Jednokratni EC2 Spot |
| `gcp` | Standardni VM | Spot VM |
| `azure` | Standardni VM | Spot VM |

## Lokalizacija

Izvršne datoteke `webhook` i `installation-router` uključuju ugrađene prijevode za 48 jezičnih oznaka (20 Tier 1, 15 afričkih, 10 azijskih i 3 južnoslavenske) u paketu `internal/i18n`. Odabir jezika poštuje varijable `LC_ALL`, `LC_MESSAGES`, `LANG` i zastavicu `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Razvoj i licencija

```bash
go test ./...
make build
```

Pojedinosti o uvođenju i radu nalaze se u cjelovitoj [engleskoj inačici README](../../../README.md). Objavljeno pod licencijom [MIT](../../../LICENSE).
