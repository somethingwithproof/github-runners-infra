> [English](../../../README.md)

# Мультиоблачные JIT-раннеры GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Go-контроллер, создающий одноразовые эфемерные раннеры GitHub Actions уровня репозитория на виртуальных машинах DigitalOcean, AWS EC2, Google Compute Engine или Azure.

## Архитектура безопасности

- Проверяет HMAC-подписи вебхуков, идентификаторы установок, идентификаторы доставок и списки разрешённых репозиториев.
- Надёжно сохраняет доставки перед подтверждением и перед выделением ресурсов проверяет через GitHub Actions API, что задания всё ещё находятся в очереди.
- Генерирует одноразовые конфигурации JIT-раннеров GitHub уровня репозитория и перед созданием помечает каждую виртуальную машину тегом принадлежности контроллеру.
- Удаляет только экземпляры, принадлежащие контроллеру, после завершения задания или истечения срока жизни (TTL).

## Поддерживаемые провайдеры

| `COMPUTE_PROVIDER` | Стандартная мощность | Прерываемая мощность |
| --- | --- | --- |
| `digitalocean` | Droplet | Недоступно (`RUNNER_SPOT=true` завершается ошибкой по принципу fail-closed) |
| `aws` | EC2 On-Demand | Одноразовый EC2 Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Локализация

Исполняемые файлы `webhook` и `installation-router` включают встроенные переводы для 45 локалей (20 локалей Tier 1, 15 африканских и 10 азиатских) в `internal/i18n`. Выбор локали учитывает `LC_ALL`, `LC_MESSAGES`, `LANG` и `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Разработка и лицензия

```bash
go test ./...
make build
```

Подробности по развёртыванию и эксплуатации см. в полной [английской версии README](../../../README.md). Распространяется под лицензией [MIT](../../../LICENSE).
