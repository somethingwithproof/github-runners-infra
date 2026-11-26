> [English](../../../README.md)

# Multi-wolk GitHub Actions JIT-uitvoerders

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lisensie](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

'n Go-beheerder wat enkeltaak-, bewaarplek-beperkte kortstondige GitHub Actions-uitvoerders op DigitalOcean, AWS EC2, Google Compute Engine of Azure VM's skep.

## Sekuriteitsargitektuur

- Verifieer webhook-HMAC-handtekeninge, installasie-ID's, aflewerings-ID's en bewaarplek-toelaatlyste.
- Stoor aflewerings duursaam voor bevestiging en verifieer dat take steeds via die GitHub Actions API in die tou staan voordat voorsiening begin.
- Genereer eenmalige, bewaarplek-beperkte GitHub JIT-uitvoerderkonfigurasies en merk elke VM met beheerder-eienaarskap voor skepping.
- Skrap slegs beheerder-beheerde instansies ná taakvoltooiing of TTL-verstryking.

## Ondersteunde verskaffers

| `COMPUTE_PROVIDER` | Standaardkapasiteit | Onderbreekbare kapasiteit |
| --- | --- | --- |
| `digitalocean` | Droplet | Nie beskikbaar nie (`RUNNER_SPOT=true` faal geslote) |
| `aws` | EC2 On-Demand | EC2 eenmalige Spot |
| `gcp` | Standaard-VM | Spot-VM |
| `azure` | Standaard-VM | Spot-VM |

## Lokalisering

Die `webhook`- en `installation-router`-binêre lêers sluit vertalings vir 45 locales (20 Vlak 1-, 15 Afrika- en 10 Asiatiese locales) in `internal/i18n` in. Locale-keuse respekteer `LC_ALL`, `LC_MESSAGES`, `LANG` en `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Ontwikkeling en lisensie

```bash
go test ./...
make build
```

Sien die volledige [Engelse README](../../../README.md) vir ontplooiings- en bedryfsbesonderhede. Gelisensieer onder [MIT](../../../LICENSE).
