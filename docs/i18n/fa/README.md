> [English](../../../README.md)

# رانرهای JIT چندابری GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![مجوز](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

یک کنترلر Go که رانرهای موقت تک‌کاره و محدود به مخزن را برای GitHub Actions روی ماشین‌های مجازی DigitalOcean، AWS EC2، Google Compute Engine یا Azure ایجاد می‌کند.

## معماری امنیتی

- امضاهای HMAC وب‌هوک، شناسه‌های نصب، شناسه‌های تحویل و فهرست‌های مجاز مخازن را تایید می‌کند.
- تحویل‌ها را پیش از تایید دریافت به‌صورت ماندگار ذخیره می‌کند و پیش از تدارک منابع، از طریق GitHub Actions API بررسی می‌کند که کارها همچنان در صف قرار داشته باشند.
- پیکربندی‌های یک‌بارمصرف و محدود به مخزن را برای رانر JIT گیت‌هاب تولید کرده و پیش از ساخت، هر ماشین مجازی را با برچسب مالکیت کنترلر نشانه‌گذاری می‌کند.
- پس از اتمام کار یا انقضای TTL، تنها نمونه‌های متعلق به کنترلر را حذف می‌کند.

## ارائه‌دهندگان پشتیبانی‌شده

| `COMPUTE_PROVIDER` | ظرفیت استاندارد | ظرفیت قابل وقفه |
| --- | --- | --- |
| `digitalocean` | Droplet | در دسترس نیست (`RUNNER_SPOT=true` با خطای ایمن متوقف می‌شود) |
| `aws` | EC2 On-Demand | EC2 Spot یک‌باره |
| `gcp` | ماشین مجازی استاندارد | ماشین مجازی Spot |
| `azure` | ماشین مجازی استاندارد | ماشین مجازی Spot |

## بومی‌سازی

فایل‌های باینری `webhook` و `installation-router` ترجمه‌های ۴۵ زبان محلی (۲۰ زبان رده اول، ۱۵ زبان آفریقایی و ۱۰ زبان آسیایی) را در `internal/i18n` در خود جای داده‌اند. انتخاب زبان از `LC_ALL`، `LC_MESSAGES`، `LANG` و `--lang` پیروی می‌کند:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## توسعه و مجوز

```bash
go test ./...
make build
```

برای جزئیات استقرار و عملیات به [نسخه انگلیسی README](../../../README.md) مراجعه کنید. منتشرشده تحت مجوز [MIT](../../../LICENSE).
