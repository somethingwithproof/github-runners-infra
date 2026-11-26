> [English](../../../README.md)

# Wielochmurowe runnery JIT dla GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licencja](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Kontroler w języku Go tworzący jednorazowe, efemeryczne runnery GitHub Actions o zakresie ograniczonym do repozytorium na maszynach wirtualnych DigitalOcean, AWS EC2, Google Compute Engine lub Azure.

## Architektura bezpieczeństwa

- Weryfikuje podpisy HMAC webhooków, identyfikatory instalacji, identyfikatory dostarczeń oraz listy dozwolonych repozytoriów.
- Trwale zapisuje otrzymane zdarzenia przed wysłaniem potwierdzenia i weryfikuje przez GitHub Actions API, czy zadania nadal oczekują w kolejce przed uruchomieniem zasobów.
- Generuje jednorazowe konfiguracje runnerów JIT GitHub o zakresie repozytorium i oznacza każdą maszynę wirtualną tagiem własności kontrolera przed jej utworzeniem.
- Usuwa wyłącznie instancje należące do kontrolera po zakończeniu zadania lub wygaśnięciu czasu TTL.

## Obsługiwani dostawcy

| `COMPUTE_PROVIDER` | Zasoby standardowe | Zasoby przerywalne |
| --- | --- | --- |
| `digitalocean` | Droplet | Niedostępne (`RUNNER_SPOT=true` kończy się bezpieczną odmową) |
| `aws` | EC2 On-Demand | Jednorazowy EC2 Spot |
| `gcp` | Standardowa VM | Spot VM |
| `azure` | Standardowa VM | Spot VM |

## Lokalizacja

Pliki binarne `webhook` i `installation-router` zawierają wbudowane tłumaczenia dla 45 ustawień regionalnych (20 z grupy Tier 1, 15 afrykańskich i 10 azjatyckich) w pakiecie `internal/i18n`. Wybór języka uwzględnia zmienne `LC_ALL`, `LC_MESSAGES`, `LANG` oraz flagę `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Rozwój i licencja

```bash
go test ./...
make build
```

Szczegółowe informacje o wdrażaniu i eksploatacji znajdują się w pełnej [angielskiej wersji README](../../../README.md). Udostępniono na licencji [MIT](../../../LICENSE).
