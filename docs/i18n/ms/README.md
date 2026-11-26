> [English](../../../README.md)

# Pelaksana JIT GitHub Actions Berbilang Awan

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lesen](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Pengendali Go yang mencipta pelaksana GitHub Actions sementara berskop repositori untuk satu tugas pada DigitalOcean, AWS EC2, Google Compute Engine, atau VM Azure.

## Seni Bina Keselamatan

- Mengesahkan tandatangan HMAC webhook, ID pemasangan, ID penghantaran, dan senarai dibenarkan repositori.
- Menyimpan penghantaran secara kekal sebelum pengakuan dan mengesahkan bahawa tugas masih beratur melalui API GitHub Actions sebelum peruntukan.
- Menjana konfigurasi pelaksana JIT GitHub berskop repositori sekali guna dan menandakan setiap VM dengan pemilikan pengendali sebelum penciptaan.
- Memadamkan tika milik pengendali sahaja selepas tugas selesai atau tamat tempoh TTL.

## Penyedia yang Disokong

| `COMPUTE_PROVIDER` | Kapasiti standard | Kapasiti boleh gangguan |
| --- | --- | --- |
| `digitalocean` | Droplet | Tidak tersedia (`RUNNER_SPOT=true` gagal secara tertutup) |
| `aws` | EC2 On-Demand | EC2 Spot sekali sahaja |
| `gcp` | VM Standard | VM Spot |
| `azure` | VM Standard | VM Spot |

## Penyetempatan

Binari `webhook` dan `installation-router` membenamkan terjemahan untuk 45 lokal (20 lokal Peringkat 1, 15 lokal Afrika, dan 10 lokal Asia) dalam `internal/i18n`. Pemilihan lokal mematuhi `LC_ALL`, `LC_MESSAGES`, `LANG`, dan `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Pembangunan dan Lesen

```bash
go test ./...
make build
```

Lihat [README Bahasa Inggeris](../../../README.md) penuh untuk butiran pengerahan dan operasi. Dilesenkan di bawah [MIT](../../../LICENSE).
