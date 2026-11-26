> [English](../../../README.md)

# Višeoblačni JIT pokretači za GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licenca](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Go kontroler koji kreira jednokratne, efemerne GitHub Actions pokretače ograničene na repozitorij na virtualnim računarima u DigitalOcean, AWS EC2, Google Compute Engine ili Azure okruženju.

## Sigurnosna arhitektura

- Provjerava HMAC potpise webhook zahtjeva, identifikatore instalacija, identifikatore isporuka i liste dozvoljenih repozitorija.
- Trajno zapisuje primljene događaje prije slanja potvrde i provjerava putem GitHub Actions API-ja da li su poslovi još uvijek u redu čekanja prije dodjele resursa.
- Generiše jednokratne GitHub JIT postavke ograničene na repozitorij i označava svaki virtualni računar oznakom vlasništva kontrolera prije kreiranja.
- Briše isključivo instance u vlasništvu kontrolera nakon završetka posla ili isteka TTL roka.

## Podržani provajderi

| `COMPUTE_PROVIDER` | Standardni kapacitet | Prekidivi kapacitet |
| --- | --- | --- |
| `digitalocean` | Droplet | Nije dostupno (`RUNNER_SPOT=true` sigurno odbija pokretanje) |
| `aws` | EC2 On-Demand | Jednokratni EC2 Spot |
| `gcp` | Standardni VM | Spot VM |
| `azure` | Standardni VM | Spot VM |

## Lokalizacija

Izvršne datoteke `webhook` i `installation-router` sadrže ugrađene prijevode za 48 jezičkih oznaka (20 Tier 1, 15 afričkih, 10 azijskih i 3 južnoslavenske) u paketu `internal/i18n`. Izbor jezika prati varijable `LC_ALL`, `LC_MESSAGES`, `LANG` i zastavicu `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Razvoj i licenca

```bash
go test ./...
make build
```

Detaljne upute za postavljanje i rad nalaze se u punoj [engleskoj verziji README](../../../README.md). Objavljeno pod licencom [MIT](../../../LICENSE).
