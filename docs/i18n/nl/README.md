> [English](../../../README.md)

# Multi-cloud GitHub Actions JIT-runners

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licentie](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Een Go-controller die eenmalige, kortstondige en tot een repository beperkte GitHub Actions-runners aanmaakt op DigitalOcean, AWS EC2, Google Compute Engine of Azure VMs.

## Beveiligingsarchitectuur

- Verifieert HMAC-handtekeningen van webhooks, installatie-ID's, leverings-ID's en toelatingslijsten (allowlists) voor repositories.
- Slaat leveringen duurzaam op vóór bevestiging en verifieert via de GitHub Actions API of jobs nog in de wachtrij staan voordat resources worden geprovisioned.
- Genereert eenmalige, tot een repository beperkte GitHub JIT-runnerconfiguraties en tagt elke VM vóór aanmaak met controller-eigendom.
- Verwijdert uitsluitend door de controller beheerde instanties na voltooiing van de job of het verstrijken van de TTL.

## Ondersteunde providers

| `COMPUTE_PROVIDER` | Standaardcapaciteit | Onderbreekbare capaciteit |
| --- | --- | --- |
| `digitalocean` | Droplet | Niet beschikbaar (`RUNNER_SPOT=true` faalt gesloten) |
| `aws` | EC2 On-Demand | EC2 eenmalige Spot |
| `gcp` | Standaard-VM | Spot-VM |
| `azure` | Standaard-VM | Spot-VM |

## Lokalisatie

De binaries `webhook` en `installation-router` bevatten ingebouwde vertalingen voor 45 locales (20 Tier 1-, 15 Afrikaanse en 10 Aziatische locales) in `internal/i18n`. De selectie van de locale respecteert `LC_ALL`, `LC_MESSAGES`, `LANG` en `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Ontwikkeling en licentie

```bash
go test ./...
make build
```

Zie de volledige [Engelse README](../../../README.md) voor details over implementatie en beheer. Gelicentieerd onder [MIT](../../../LICENSE).
