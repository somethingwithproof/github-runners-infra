> [English](../../../README.md)

# مشغّلات GitHub Actions الفورية (JIT Runners) متعددة السحابات

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

متحكم (controller) بلغة Go بينشئ مشغّلات GitHub Actions مؤقتة لمهمة واحدة ومحدودة بمستودع معيّن على DigitalOcean أو AWS EC2 أو Google Compute Engine أو أجهزة Azure الافتراضية (Azure VMs).

## البنية الأمنية

- بيتحقق من تواقيع HMAC للـ webhook، ومعرّفات التثبيت (installation IDs)، ومعرّفات التسليم (delivery IDs)، وقوائم السماح للمستودعات (repository allowlists).
- بيحفظ عمليات التسليم بشكل دائم قبل إرسال التأكيد (acknowledgement)، وبيتحقق إن المهام (jobs) لسه في قائمة الانتظار عن طريق GitHub Actions API قبل تجهيز الموارد.
- بيولّد إعدادات مشغّل GitHub JIT للاستخدام مرة واحدة ومحدودة بالمستودع، وبيعلّم كل VM بوسم ملكية المتحكم قبل الإنشاء.
- بيمسح فقط الـ instances المملوكة للمتحكم بعد انتهاء المهمة أو انتهاء مدة الصلاحية (TTL).

## مزوّدو الخدمة المدعومون

| `COMPUTE_PROVIDER` | السعة القياسية | السعة القابلة للمقاطعة (Interruptible) |
| --- | --- | --- |
| `digitalocean` | Droplet | غير متاح (`RUNNER_SPOT=true` يرفض التشغيل بأمان / fail closed) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## اللغات والترجمة

الملفات التنفيذية `webhook` و`installation-router` مدمج جواها ترجمات لـ 45 لغة وإعداد محلي (20 من الفئة الأولى Tier 1، و15 لغة أفريقية، و10 لغات آسيوية) في `internal/i18n`. اختيار اللغة بيتبع `LC_ALL` و`LC_MESSAGES` و`LANG` و`--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## التطوير والترخيص

```bash
go test ./...
make build
```

شوف [ملف README الإنجليزي](../../../README.md) الكامل لتفاصيل النشر والتشغيل. مرخّص بموجب [MIT](../../../LICENSE).
