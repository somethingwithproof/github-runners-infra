> [English](../../../README.md)

# Çoklu Bulut GitHub Actions JIT Runner Denetleyicisi

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Lisans](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

DigitalOcean, AWS EC2, Google Compute Engine veya Azure sanal makineleri üzerinde tek işlik, depo kapsamlı geçici GitHub Actions runner'ları oluşturan bir Go denetleyicisidir.

## Güvenlik mimarisi

- Webhook HMAC imzalarını, kurulum kimliklerini (installation ID), teslimat kimliklerini (delivery ID) ve depo izin listelerini doğrular.
- Onay yanıtı dönmeden önce teslimatları kalıcı olarak saklar ve kaynak sağlamadan önce işlerin hala kuyrukta olduğunu GitHub Actions API üzerinden doğrular.
- Tek kullanımlık, depo kapsamlı GitHub JIT runner yapılandırmaları üretir ve oluşturma öncesinde her sanal makineyi denetleyici sahipliğiyle etiketler.
- İş tamamlandıktan veya TTL süresi dolduktan sonra yalnızca denetleyiciye ait örnekleri siler.

## Desteklenen sağlayıcılar

| `COMPUTE_PROVIDER` | Standart kapasite | Kesintiye uğratılabilir kapasite |
| --- | --- | --- |
| `digitalocean` | Droplet | Kullanılamaz (`RUNNER_SPOT=true` güvenli biçimde başarısız olur) |
| `aws` | EC2 On-Demand | EC2 tek seferlik Spot |
| `gcp` | Standart VM | Spot VM |
| `azure` | Standart VM | Spot VM |

## Yerelleştirme

`webhook` ve `installation-router` ikili dosyaları, `internal/i18n` içinde 45 yerel ayar (20 Tier 1, 15 Afrika ve 10 Asya yerel ayarı) için çevirileri gömülü olarak içerir. Yerel ayar seçimi `LC_ALL`, `LC_MESSAGES`, `LANG` değişkenlerini ve `--lang` bayrağını dikkate alır:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Geliştirme ve Lisans

```bash
go test ./...
make build
```

Dağıtım ve işletim ayrıntıları için tam [İngilizce README](../../../README.md) dosyasına bakın. [MIT](../../../LICENSE) altında lisanslanmıştır.
