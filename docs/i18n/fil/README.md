> [English](../../../README.md)

# Multi-cloud na mga GitHub Actions JIT Runner

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lisensya](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Isang Go controller na gumagawa ng pang-isahang-job, repository-scoped na ephemeral GitHub Actions runner sa DigitalOcean, AWS EC2, Google Compute Engine, o mga Azure VM.

## Arkitekturang pangseguridad

- Bineberipika ang mga webhook HMAC signature, installation ID, delivery ID, at mga repository allowlist.
- Permanenteng itinatala ang mga delivery bago mag-acknowledge at bineberipika kung naka-queue pa rin ang mga job sa pamamagitan ng GitHub Actions API bago mag-provision.
- Gumagawa ng mga pang-isahang-gamit at repository-scoped na GitHub JIT runner configuration at nilalagyan ng tag ng pagmamay-ari ng controller ang bawat VM bago likhain.
- Binubura lamang ang mga instance na pagmamay-ari ng controller pagkatapos makumpleto ang job o mag-expire ang TTL.

## Mga sinusuportahang provider

| `COMPUTE_PROVIDER` | Karaniwang kapasidad | Naaantala (interruptible) na kapasidad |
| --- | --- | --- |
| `digitalocean` | Droplet | Hindi available (nagfe-fail closed ang `RUNNER_SPOT=true`) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Lokalisasyon

Nag-e-embed ang mga binary na `webhook` at `installation-router` ng mga salin para sa 45 locale (20 Tier 1, 15 African, at 10 Asian na locale) sa `internal/i18n`. Sinusunod ng pagpili ng locale ang `LC_ALL`, `LC_MESSAGES`, `LANG`, at `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Pag-develop at Lisensya

```bash
go test ./...
make build
```

Tingnan ang buong [README sa Ingles](../../../README.md) para sa mga detalye ng deployment at operasyon. Lisensyado sa ilalim ng [MIT](../../../LICENSE).
