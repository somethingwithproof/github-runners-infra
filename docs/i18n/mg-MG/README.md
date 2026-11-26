> [English](../../../README.md)

# GitHub Actions JIT Runners amin'ny rahona maro

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Mpanara-maso Go izay mamorona mpampandeha GitHub Actions mandalo ho an'ny asa tokana sy voafetra ho an'ny tahiry iray ao amin'ny DigitalOcean, AWS EC2, Google Compute Engine, na Azure VMs.

## Rafitra fiarovana

- Manamarina ny sonia HMAC an'ny webhook, ny ID fametrahana, ny ID fandefasana, ary ny lisitry ny tahiry nahazo alalana.
- Mitahiry maharitra ny fandefasana alohan'ny fanamafisana ny fandraisana ary manamarina amin'ny alalan'ny GitHub Actions API fa mbola ao anaty filaharana ny asa alohan'ny fanomanana.
- Mamorona fikirakirana mpampandeha JIT GitHub ampiasaina indray mandeha ho an'ny tahiry iray ary manisy marika ny VM tsirairay ho fananan'ny mpanara-maso alohan'ny famoronana.
- Mamafa ireo singa fananan'ny mpanara-maso ihany rehefa vita ny asa na tapitra ny fe-potoana TTL.

## Mpanome tolotra tohanana

| `COMPUTE_PROVIDER` | Fahafahana mahazatra | Fahafahana azo tapahina |
| --- | --- | --- |
| `digitalocean` | Droplet | Tsy misy (`RUNNER_SPOT=true` dia mandà sy mijanona) |
| `aws` | EC2 On-Demand | EC2 Spot indray mandeha |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Fampifanarahana amin'ny fiteny

Ireo programa `webhook` sy `installation-router` dia ahitana fandikan-teny ho an'ny fiteny eo an-toerana miisa 45 (fiteny sokajy 1 miisa 20, fiteny afrikanina miisa 15, ary fiteny aziatika miisa 10) ao amin'ny `internal/i18n`. Ny fisafidianana ny fiteny dia manaja ny `LC_ALL`, `LC_MESSAGES`, `LANG`, ary `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Fampandrosoana sy Lisansa

```bash
go test ./...
make build
```

Jereo ny [README amin'ny teny anglisy](../../../README.md) feno ho an'ny antsipiriany momba ny fametrahana sy ny fampandehanana. Navoaka teo ambany lisansa [MIT](../../../LICENSE).
