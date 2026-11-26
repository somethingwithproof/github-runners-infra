> [English](../../../README.md)

# Runner JIT GitHub Actions Multi-cloud

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lisensi](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Kontroler Go yang membuat runner GitHub Actions sementara berskala repositori untuk satu job pada VM DigitalOcean, AWS EC2, Google Compute Engine, atau Azure.

## Arsitektur keamanan

- Memverifikasi tanda tangan HMAC webhook, ID instalasi, ID pengiriman, dan daftar izin repositori.
- Menyimpan data pengiriman secara persisten sebelum mengirim konfirmasi dan memverifikasi bahwa job masih berada dalam antrean melalui API GitHub Actions sebelum melakukan provisi.
- Menghasilkan konfigurasi runner JIT GitHub sekali pakai berskala repositori dan menandai setiap VM dengan kepemilikan kontroler sebelum pembuatan.
- Hanya menghapus instans milik kontroler setelah job selesai atau masa berlaku TTL berakhir.

## Penyedia yang didukung

| `COMPUTE_PROVIDER` | Kapasitas standar | Kapasitas yang dapat diinterupsi |
| --- | --- | --- |
| `digitalocean` | Droplet | Tidak tersedia (`RUNNER_SPOT=true` gagal secara fail-closed) |
| `aws` | EC2 On-Demand | EC2 Spot sekali pakai |
| `gcp` | VM Standar | VM Spot |
| `azure` | VM Standar | VM Spot |

## Pelokalan

Biner `webhook` dan `installation-router` menyertakan terjemahan untuk 45 lokal (20 lokal Tier 1, 15 lokal Afrika, dan 10 lokal Asia) di `internal/i18n`. Pemilihan lokal mengikuti `LC_ALL`, `LC_MESSAGES`, `LANG`, dan `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Pengembangan dan Lisensi

```bash
go test ./...
make build
```

Lihat [README bahasa Inggris](../../../README.md) lengkap untuk detail penerapan dan operasi. Dilisensikan di bawah [MIT](../../../LICENSE).
