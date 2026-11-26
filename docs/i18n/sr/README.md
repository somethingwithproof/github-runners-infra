> [English](../../../README.md)

# Вишеоблачни JIT извршиоци за GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Лиценца](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Go контролер који креира једнократне, пролазне GitHub Actions извршиоце ограничене на репозиторијум на виртуелним рачунарима у DigitalOcean, AWS EC2, Google Compute Engine или Azure окружењу.

## Безбедносна архитектура

- Проверава HMAC потписе webhook захтева, идентификаторе инсталација, идентификаторе испорука и листе дозвољених репозиторијума.
- Трајно чува примљене догађаје пре слања потврде и проверава путем GitHub Actions API-ја да ли су послови још у реду чекања пре доделе ресурса.
- Генерише једнократна GitHub JIT подешавања ограничена на репозиторијум и означава сваки виртуелни рачунар ознаком власништва контролера пре креирања.
- Брише искључиво инстанце у власништву контролера након завршетка посла или истека TTL рока.

## Подржани провајдери

| `COMPUTE_PROVIDER` | Стандардни капацитет | Прекидиви капацитет |
| --- | --- | --- |
| `digitalocean` | Droplet | Није доступно (`RUNNER_SPOT=true` безбедно одбија покретање) |
| `aws` | EC2 On-Demand | Једнократни EC2 Spot |
| `gcp` | Стандардни VM | Spot VM |
| `azure` | Стандардни VM | Spot VM |

## Локализација

Извршне датотеке `webhook` и `installation-router` садрже уграђене преводе за 48 језичких ознака (20 Tier 1, 15 афричких, 10 азијских и 3 јужнословенске) у пакету `internal/i18n`. Избор језика прати променљиве `LC_ALL`, `LC_MESSAGES`, `LANG` и заставицу `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Развој и лиценца

```bash
go test ./...
make build
```

Детаљна упутства за примену и рад налазе се у потпуној [енглеској верзији README](../../../README.md). Објављено под лиценцом [MIT](../../../LICENSE).
