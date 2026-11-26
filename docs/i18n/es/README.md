> [English](../../../README.md)

# Ejecutores JIT multinube para GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licencia](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Un controlador en Go que crea ejecutores efímeros de GitHub Actions de un solo trabajo y con ámbito de repositorio en DigitalOcean, AWS EC2, Google Compute Engine o máquinas virtuales de Azure.

## Arquitectura de seguridad

- Verifica las firmas HMAC de los webhooks, los ID de instalación, los ID de entrega y las listas de repositorios permitidos.
- Almacena las entregas de forma duradera antes de confirmar la recepción y verifica mediante la API de GitHub Actions que los trabajos sigan en cola antes del aprovisionamiento.
- Genera configuraciones de ejecutores JIT de GitHub de un solo uso y con ámbito de repositorio, y etiqueta cada máquina virtual con la propiedad del controlador antes de su creación.
- Elimina únicamente las instancias propiedad del controlador tras la finalización del trabajo o el vencimiento del TTL.

## Proveedores compatibles

| `COMPUTE_PROVIDER` | Capacidad estándar | Capacidad interrumpible |
| --- | --- | --- |
| `digitalocean` | Droplet | No disponible (`RUNNER_SPOT=true` falla de forma segura) |
| `aws` | EC2 On-Demand | EC2 Spot de un solo uso |
| `gcp` | Máquina virtual estándar | Máquina virtual Spot |
| `azure` | Máquina virtual estándar | Máquina virtual Spot |

## Localización

Los binarios `webhook` y `installation-router` integran traducciones para 45 configuraciones regionales (20 de nivel 1, 15 africanas y 10 asiáticas) en `internal/i18n`. La selección de configuración regional respeta `LC_ALL`, `LC_MESSAGES`, `LANG` y `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Desarrollo y licencia

```bash
go test ./...
make build
```

Consulta el [README completo en inglés](../../../README.md) para obtener detalles de implementación y operación. Publicado bajo la licencia [MIT](../../../LICENSE).
