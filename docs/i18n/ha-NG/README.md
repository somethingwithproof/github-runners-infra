> [English](../../../README.md)

# Masu Gudanarwa na GitHub Actions JIT na Gajimare Masu Yawa

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lasisi](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Mai sarrafawa na Go wanda ke ƙirƙirar masu gudanarwa na GitHub Actions masu ɗan lokaci don aiki guda ɗaya a iyakar ma'ajiyar lamba a kan DigitalOcean, AWS EC2, Google Compute Engine, ko Azure VMs.

## Tsarin Tsaro

- Yana tabbatar da sa hannun HMAC na webhook, lambobin shigarwa, lambobin isarwa, da jerin ma'ajiyoyin da aka yarda da su.
- Yana adana bayanan isarwa na dindindin kafin tabbatarwa kuma yana tabbatar da cewa ayyukan har yanzu suna kan layi ta hanyar GitHub Actions API kafin samar da na'ura.
- Yana samar da saitunan mai gudanarwa na GitHub JIT masu amfani sau ɗaya a iyakar ma'ajiya kuma yana yi wa kowane VM alamar mallakar mai sarrafawa kafin ƙirƙira.
- Yana goge na'urorin da mai sarrafawa kawai ya mallaka bayan kammala aiki ko ƙarewar TTL.

## Masu Ba da Sabis da Ake Tallafawa

| `COMPUTE_PROVIDER` | Ƙarfi na yau da kullum | Ƙarfi mai katsewa |
| --- | --- | --- |
| `digitalocean` | Droplet | Ba ya samuwa (`RUNNER_SPOT=true` yana rufewa ba tare da ci gaba ba) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Daidaita Harshe

Fayilolin `webhook` da `installation-router` suna ɗauke da fassarori don harsuna 45 (20 na Mataki na 1, 15 na Afirka, da 10 na Asiya) a cikin `internal/i18n`. Zaɓin harshe yana mutunta `LC_ALL`, `LC_MESSAGES`, `LANG`, da `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Haɓakawa da Lasisi

```bash
go test ./...
make build
```

Duba cikakken [README na Turanci](../../../README.md) don cikakkun bayanai kan turawa da gudanarwa. An ba da lasisi ƙarƙashin [MIT](../../../LICENSE).
