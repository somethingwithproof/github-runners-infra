> [English](../../../README.md)

# Abakoresha ba GitHub Actions JIT kuri Multi-cloud

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Umugenzuzi wa Go ukora abakoresha ba GitHub Actions b'akanya gato, bakora umurimo umwe gusa kandi bagenewe ububiko bumwe kuri DigitalOcean, AWS EC2, Google Compute Engine, cyangwa Azure VMs.

## Imiterere y'umutekano

- Igenzura imikono ya HMAC ya webhook, ID z'iyinjizwa, ID z'iyoherezwa, n'urutonde rw'ububiko bwemewe.
- Ibika iyoherezwa mu buryo burambye mbere yo kwemeza ko ryakiriwe kandi ikagenzura ko imirimo igitegereje ku murongo ikoresheje GitHub Actions API mbere yo gutanga imashini.
- Ikora igenamiterere rya GitHub JIT runner rikoreshwa rimwe gusa rigenewe ububiko bumwe kandi igashyira ikimenyetso cy'ubuyobozi bw'umugenzuzi kuri buri VM mbere yo kuyikora.
- Isiba gusa imashini zifitwe n'umugenzuzi nyuma y'uko umurimo urangiye cyangwa igihe cya TTL kirangiye.

## Abatanga serivisi bemewe

| `COMPUTE_PROVIDER` | Ubushobozi busanzwe | Ubushobozi bushobora guhagarikwa |
| --- | --- | --- |
| `digitalocean` | Droplet | Ntibiboneka (`RUNNER_SPOT=true` irahagarara itatanze serivisi) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Ishyirwa mu ndimi

Porogaramu za `webhook` na `installation-router` zirimo ubusobanuro bw'indimi 45 (indimi 20 z'icyiciro cya 1, 15 zo muri Afurika, na 10 zo muri Aziya) muri `internal/i18n`. Ihitamo ry'ururimi rikurikiza `LC_ALL`, `LC_MESSAGES`, `LANG`, na `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Iterambere n'Uruhushya

```bash
go test ./...
make build
```

Reba [README yuzuye mu Cyongereza](../../../README.md) kugira ngo ubone amakuru arambuye ku ishyirwa mu bikorwa n'imikorere. Ifite uruhushya rwa [MIT](../../../LICENSE).
