> [English](../../../README.md)

# Waendeshaji wa JIT wa GitHub Actions katika Mawingu Mengi

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Kidhibiti cha Go kinachounda waendeshaji wa muda mfupi wa GitHub Actions wa kazi moja na wenye mipaka ya hazina kwenye DigitalOcean, AWS EC2, Google Compute Engine, au VM za Azure.

## Muundo wa usalama

- Huthibitisha saini za HMAC za webhook, vitambulisho vya usakinishaji, vitambulisho vya uwasilishaji, na orodha za ruhusa za hazina.
- Huhifadhi uwasilishaji kwa kudumu kabla ya kuthibitisha upokeaji na huthibitisha kuwa kazi bado ziko kwenye foleni kupitia API ya GitHub Actions kabla ya kuandaa rasilimali.
- Huunda usanidi wa mwendeshaji wa JIT wa GitHub wa matumizi ya mara moja wenye mipaka ya hazina na kuweka lebo ya umiliki wa kidhibiti kwenye kila VM kabla ya kuiunda.
- Hufuta tu matukio yanayomilikiwa na kidhibiti baada ya kazi kukamilika au muda wa TTL kuisha.

## Watoa huduma wanaotumika

| `COMPUTE_PROVIDER` | Uwezo wa kawaida | Uwezo unaoweza kukatizwa |
| --- | --- | --- |
| `digitalocean` | Droplet | Haipatikani (`RUNNER_SPOT=true` hushindwa kwa usalama) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Ujanibishaji

Faili tekelezi za `webhook` na `installation-router` hujumuisha tafsiri za lugha na maeneo 45 (maeneo 20 ya Daraja la 1, 15 ya Afrika, na 10 ya Asia) katika `internal/i18n`. Uteuzi wa eneo huheshimu `LC_ALL`, `LC_MESSAGES`, `LANG`, na `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Uendelezaji na Leseni

```bash
go test ./...
make build
```

Tazama [README kamili ya Kiingereza](../../../README.md) kwa maelezo ya upelekaji na uendeshaji. Imepewa leseni chini ya [MIT](../../../LICENSE).
