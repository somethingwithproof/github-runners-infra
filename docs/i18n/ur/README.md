> [English](../../../README.md)

# ملٹی کلاؤڈ GitHub Actions JIT رنرز

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

ایک Go کنٹرولر جو DigitalOcean، AWS EC2، Google Compute Engine، یا Azure VMs پر سنگل جاب، ریپوزٹری کے دائرہ کار تک محدود عارضی GitHub Actions رنرز بناتا ہے۔

## سیکیورٹی آرکیٹیکچر

- ویب ہک HMAC دستخطوں، انسٹالیشن IDs، ڈیلیوری IDs، اور ریپوزٹری الاؤ لسٹس کی تصدیق کرتا ہے۔
- اعترافِ وصول سے پہلے ڈیلیوریز کو مستقل طور پر محفوظ کرتا ہے اور پروویژننگ سے پہلے GitHub Actions API کے ذریعے تصدیق کرتا ہے کہ جابز اب بھی قطار میں ہیں۔
- ایک بار استعمال ہونے والی ریپوزٹری تک محدود GitHub JIT رنر کنفیگریشنز تیار کرتا ہے اور تخلیق سے پہلے ہر VM کو کنٹرولر کی ملکیت کے ساتھ ٹیگ کرتا ہے۔
- جاب مکمل ہونے یا TTL کی مدت ختم ہونے کے بعد صرف کنٹرولر کی ملکیت والے انسٹینسز کو حذف کرتا ہے۔

## معاون فراہم کنندگان

| `COMPUTE_PROVIDER` | معیاری گنجائش | منقطع ہونے کے قابل گنجائش |
| --- | --- | --- |
| `digitalocean` | Droplet | دستیاب نہیں (`RUNNER_SPOT=true` ناکام ہو کر بند ہو جاتا ہے) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## لوکلائزیشن

`webhook` اور `installation-router` بائنریز `internal/i18n` میں 45 لوکیلز (20 ٹیئر 1، 15 افریقی، اور 10 ایشیائی لوکیلز) کے لیے تراجم شامل کرتی ہیں۔ لوکیل کا انتخاب `LC_ALL`، `LC_MESSAGES`، `LANG` اور `--lang` کا احترام کرتا ہے:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## ڈیولپمنٹ اور لائسنس

```bash
go test ./...
make build
```

ڈپلائمنٹ اور آپریشنز کی تفصیلات کے لیے مکمل [انگریزی README](../../../README.md) دیکھیں۔ [MIT](../../../LICENSE) کے تحت لائسنس یافتہ۔
