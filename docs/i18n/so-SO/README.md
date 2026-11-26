> [English](../../../README.md)

# Orodada JIT ee GitHub Actions ee Daruuraha Badan

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Shatiga](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Maamule Go ku qoran oo ka abuura orodada GitHub Actions ee hal-shaqo ah, ku kooban kaydka, oo ku meel gaar ah gudaha DigitalOcean, AWS EC2, Google Compute Engine, ama mashiinnada Azure VMs.

## Qaab-dhismeedka Amniga

- Wuxuu xaqiijiyaa saxiixyada HMAC ee webhook-ga, aqoonsiga rakibaadda, aqoonsiga gaarsiinta, iyo liiska kaydadka la oggol yahay.
- Wuxuu si waarta u kaydiyaa gaarsiinta ka hor inta uusan qirin wuxuuna xaqiijiyaa in shaqooyinku weli saf ku jiraan iyada oo loo marayo GitHub Actions API ka hor inta aan la diyaarin.
- Wuxuu soo saaraa qaabeynta orodaha JIT ee GitHub oo hal mar la isticmaalo kuna kooban kaydka wuxuuna ku calaamadeeyaa VM kasta lahaanshaha maamulaha ka hor abuurista.
- Wuxuu tirtiraa oo keliya mashiinnada uu maamuluhu leeyahay marka shaqadu dhammaato ama uu dhaco waqtiga TTL.

## Bixiyeyaasha la taageero

| `COMPUTE_PROVIDER` | Awoodda caadiga ah | Awoodda la joojin karo |
| --- | --- | --- |
| `digitalocean` | Droplet | Lama heli karo (`RUNNER_SPOT=true` si xiran ayay u fashilantaa) |
| `aws` | EC2 On-Demand | EC2 Spot oo hal-mar ah |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Deegaameynta

Faylasha fulinta ee `webhook` iyo `installation-router` waxay ku dhex haystaan tarjumaado loogu talagalay 45 deegaan (20 Heerka 1-aad, 15 Afrikaan, iyo 10 Aasiyaan ah) gudaha `internal/i18n`. Doorashada deegaanku waxay tixgelisaa `LC_ALL`, `LC_MESSAGES`, `LANG`, iyo `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Horumarinta iyo Shatiga

```bash
go test ./...
make build
```

Eeg [README-ga Ingiriisiga](../../../README.md) oo dhammaystiran si aad u hesho faahfaahinta daabulidda iyo hawlgalka. Waxaa lagu shatiyeeyay [MIT](../../../LICENSE).
