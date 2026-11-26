> [English](../../../README.md)

# Ndị na-agba ọsọ JIT GitHub Actions n'ọtụtụ igwe ojii

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Onye njikwa Go nke na-emepụta ndị na-agba ọsọ GitHub Actions nwa oge maka otu ọrụ naanị ma gbasaa n'ime otu ebe nchekwa na DigitalOcean, AWS EC2, Google Compute Engine, ma ọ bụ Azure VMs.

## Nhazi nchekwa

- Na-enyocha mbinye aka HMAC nke webhook, ID nrụnye, ID nnyefe, na ndepụta ebe nchekwa e kwere.
- Na-echekwa nnyefe n'ụzọ siri ike tupu nkwenye ma na-enyocha na ọrụ ka nọ n'ahịrị site na GitHub Actions API tupu inye akụrụngwa.
- Na-emepụta nhazi onye na-agba ọsọ GitHub JIT maka ojiji otu ugboro n'ime otu ebe nchekwa ma tinye akara nwe nke onye njikwa na VM ọ bụla tupu e kee ya.
- Na-ehichapụ naanị igwe ndị onye njikwa nwe mgbe ọrụ gwụchara ma ọ bụ mgbe TTL gwụrụ.

## Ndị na-enye ọrụ a kwadoro

| `COMPUTE_PROVIDER` | Ikike ọkọlọtọ | Ikike a pụrụ ịkwụsị |
| --- | --- | --- |
| `digitalocean` | Droplet | Adịghị (`RUNNER_SPOT=true` na-ada n'ụzọ mechiri emechi) |
| `aws` | EC2 On-Demand | EC2 Spot nke otu ugboro |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Nkwado asụsụ obodo

Faịlụ `webhook` na `installation-router` tinyere ntụgharị asụsụ maka asụsụ obodo 45 (asụsụ Tier 1 20, asụsụ Afrịka 15, na asụsụ Eshia 10) na `internal/i18n`. Nhọrọ asụsụ obodo na-asọpụrụ `LC_ALL`, `LC_MESSAGES`, na `LANG`, yana `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Mmepe na Ikikere

```bash
go test ./...
make build
```

Lee [README Bekee](../../../README.md) zuru ezu maka nkọwa nke ntinye n'ọrụ na arụmọrụ. E nyere ikikere n'okpuru [MIT](../../../LICENSE).
