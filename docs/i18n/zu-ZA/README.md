> [English](../../../README.md)

# Izigijimi ze-JIT ze-GitHub Actions ze-Multi-cloud

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Isilawuli se-Go esidala izigijimi ze-GitHub Actions zesikhashana zomsebenzi owodwa ezinomkhawulo we-repository ku-DigitalOcean, ku-AWS EC2, ku-Google Compute Engine, noma kuma-Azure VM.

## Isakhiwo sokuvikeleka

- Siqinisekisa amasiginesha e-HMAC we-webhook, ama-ID okufakwa, ama-ID okulethwa, kanye nezinhlu ezivunyelwe zama-repository.
- Sigcina okulethiwe ngokuhlala njalo ngaphambi kokuvuma futhi siqinisekisa ukuthi imisebenzi isalindile nge-GitHub Actions API ngaphambi kokuhlinzeka.
- Sikhiqiza ukulungiselelwa kwezigijimi ze-GitHub JIT zokusetshenziswa kanye ezinomkhawulo we-repository futhi sibhale i-VM ngayinye ngobunikazi besilawuli ngaphambi kokudalwa.
- Sisusa kuphela izibonelo ezingezisilawuli ngemva kokuphothulwa komsebenzi noma ukuphelelwa yisikhathi kwe-TTL.

## Abahlinzeki abasekelwayo

| `COMPUTE_PROVIDER` | Umthamo ojwayelekile | Umthamo onganqamuka |
| --- | --- | --- |
| `digitalocean` | Droplet | Akutholakali (`RUNNER_SPOT=true` iyehluleka ngokuphephile) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## Ukwenziwa ngokwendawo

Ama-binary e-`webhook` ne-`installation-router` afaka ukuhunyushwa kwezilimi zendawo ezingu-45 (ezingu-20 ze-Tier 1, ezingu-15 zase-Afrika, kanye nezingu-10 zase-Asia) ku-`internal/i18n`. Ukukhethwa kolimi lwendawo kuhlonipha i-`LC_ALL`, i-`LC_MESSAGES`, i-`LANG`, kanye ne-`--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Ukuthuthukiswa kanye Nelayisense

```bash
go test ./...
make build
```

Bheka [i-README yesiNgisi](../../../README.md) ephelele ngemininingwane yokufaka kanye nokusebenza. Inikezwe ilayisense ngaphansi kwe-[MIT](../../../LICENSE).
