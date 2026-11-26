> [English](../../../README.md)

# Àwọn Olùṣiṣẹ́ JIT GitHub Actions Oríṣiríṣi Àwọsánmọ̀

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Ìwé-àṣẹ](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Olùṣàkóso Go kan tí ó ń ṣẹ̀dá àwọn olùṣiṣẹ́ GitHub Actions ìgbà-díẹ̀ fún iṣẹ́ kan ṣoṣo tí ó mọ sí ibi-ìpamọ́ lórí DigitalOcean, AWS EC2, Google Compute Engine, tàbí Azure VMs.

## Ètò ààbò

- Ń ṣe ìdánilójú ìfọwọ́sí HMAC webhook, àwọn ID ìfisílẹ̀, àwọn ID ìfijiṣẹ́, àti àkójọ àwọn ibi-ìpamọ́ tí a fàyè gbà.
- Ń tọ́jú àwọn ìfijiṣẹ́ sílẹ̀ dáadáa kí ó tó gba ìjẹ́wọ́, ó sì ń rí i dájú pé àwọn iṣẹ́ ṣì wà ní ìlà nípasẹ̀ GitHub Actions API kí ó tó ṣe ìpèsè.
- Ń ṣẹ̀dá àwọn ìṣètò olùṣiṣẹ́ JIT GitHub tí a ń lò lẹ́ẹ̀kan ṣoṣo fún ibi-ìpamọ́ kan, ó sì ń fi àmì ìjẹ́-oníní olùṣàkóso sí gbogbo VM kí ó tó ṣẹ̀dá rẹ̀.
- Ń pa àwọn ẹ̀rọ tí olùṣàkóso nìkan jẹ́ oníní wọn rẹ́ lẹ́yìn tí iṣẹ́ bá parí tàbí tí àkókò TTL bá parí.

## Àwọn olùpèsè tí a ṣe àtìlẹ́yìn fún

| `COMPUTE_PROVIDER` | Agbára déédéé | Agbára tí a lè dá dúró |
| --- | --- | --- |
| `digitalocean` | Droplet | Kò sí (`RUNNER_SPOT=true` ń kùnà ní ààbò) |
| `aws` | EC2 On-Demand | EC2 Spot ẹ̀ẹ̀kan ṣoṣo |
| `gcp` | VM déédéé | Spot VM |
| `azure` | VM déédéé | Spot VM |

## Ìsọdèdè-agbègbè

Àwọn fáìlì aláṣẹ `webhook` àti `installation-router` fi àwọn ìtumọ̀ kún inú wọn fún àwọn èdè agbègbè 45 (20 ti Ìpele 1, 15 ti Áfíríkà, àti 10 ti Éṣíà) nínú `internal/i18n`. Yíyan èdè agbègbè ń tẹ̀lé `LC_ALL`, `LC_MESSAGES`, `LANG`, àti `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Ìdàgbàsókè àti Ìwé-àṣẹ

```bash
go test ./...
make build
```

Wo [README èdè Gẹ̀ẹ́sì](../../../README.md) ní kíkún fún àwọn àlàyé ìfìsílẹ̀-iṣẹ́ àti ìṣiṣẹ́. A fún un ní ìwé-àṣẹ lábẹ́ [MIT](../../../LICENSE).
