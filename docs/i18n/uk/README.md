> [English](../../../README.md)

# Мультихмарні JIT-раннери GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Ліцензія](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Контролер на Go, який створює одноразові ефемерні раннери GitHub Actions, прив'язані до одного завдання та репозиторію, на віртуальних машинах DigitalOcean, AWS EC2, Google Compute Engine або Azure.

## Архітектура безпеки

- Перевіряє HMAC-підписи вебхуків, ідентифікатори інсталяцій, ідентифікатори доставок та списки дозволених репозиторіїв.
- Надійно зберігає доставки перед підтвердженням отримання та перед виділенням ресурсів перевіряє через GitHub Actions API, що завдання все ще перебувають у черзі.
- Генерує одноразові конфігурації GitHub JIT-раннерів із областю дії на рівні репозиторію та позначає кожну віртуальну машину тегом власності контролера перед створенням.
- Видаляє лише екземпляри, що належать контролеру, після завершення завдання або закінчення TTL.

## Підтримувані провайдери

| `COMPUTE_PROVIDER` | Стандартні ресурси | Переривані ресурси |
| --- | --- | --- |
| `digitalocean` | Droplet | Недоступно (`RUNNER_SPOT=true` завершується безпечною відмовою) |
| `aws` | EC2 On-Demand | Одноразовий EC2 Spot |
| `gcp` | Стандартна ВМ | Spot ВМ |
| `azure` | Стандартна ВМ | Spot ВМ |

## Локалізація

Виконувані файли `webhook` та `installation-router` містять вбудовані переклади для 45 локалей (20 рівня Tier 1, 15 африканських і 10 азійських локалей) у `internal/i18n`. Вибір локалі враховує `LC_ALL`, `LC_MESSAGES`, `LANG` та `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Розробка та ліцензія

```bash
go test ./...
make build
```

Див. повний [README англійською](../../../README.md), щоб дізнатися деталі розгортання та експлуатації. Ліцензовано за [MIT](../../../LICENSE).
