> [English](../../../README.md)

# Abakhupheli be-JIT be-GitHub Actions kumafu amaninzi

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Ilayisensi](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Umlawuli we-Go owenza abakhupheli be-GitHub Actions bexeshana bomsebenzi omnye abaphelele kwindawo yokugcina ikhowudi enye kwi-DigitalOcean, AWS EC2, Google Compute Engine, okanye kwi-Azure VMs.

## Uyilo lokhuseleko

- Iqinisekisa iisayini ze-webhook HMAC, ii-ID zokufakela, ii-ID zokuhambisa, kunye noluhlu lwamavume eendawo zokugcina ikhowudi.
- Igcina izinto ezihanjiswayo ngokusisigxina ngaphambi kokuvuma kwaye iqinisekisa ukuba imisebenzi iseludwelisweni nge-GitHub Actions API ngaphambi kokubonelela.
- Ivelisa ubumbeko lomkhupheli we-GitHub JIT osetyenziswa kanye ophelele kwindawo yokugcina ikhowudi kwaye imakisha yonke i-VM ngobunini bomlawuli ngaphambi kokudalwa.
- Icima kuphela iimeko eziphethwe ngumlawuli emva kokugqitywa komsebenzi okanye ukuphela kwe-TTL.

## Ababoneleli abaxhaswayo

| `COMPUTE_PROVIDER` | Umthamo oqhelekileyo | Umthamo ophazamisekayo |
| --- | --- | --- |
| `digitalocean` | Droplet | Ayifumaneki (`RUNNER_SPOT=true` iyasilela ngokuvaleka) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Ulungiselelo lweelwimi

Iifayile ezimbini ze-`webhook` kunye ne-`installation-router` zifakela iinguqulelo zeelwimi zasekuhlaleni ezingama-45 (ezingama-20 ze-Tier 1, ezili-15 zase-Afrika, nezili-10 zase-Asia) kwi-`internal/i18n`. Ukukhethwa kolwimi lwasekuhlaleni kuhlonipha i-`LC_ALL`, `LC_MESSAGES`, `LANG`, kunye ne-`--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Uphuhliso neLayisensi

```bash
go test ./...
make build
```

Jonga i-[README yesiNgesi](../../../README.md) epheleleyo ukuze ufumane iinkcukacha zokusasaza nokusebenza. Inikwe ilayisensi phantsi kwe-[MIT](../../../LICENSE).
