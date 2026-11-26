> [English](../../../README.md)

# مشغلات GitHub Actions JIT متعددة السحابات

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![الترخيص](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

وحدة تحكم بلغة Go تنشئ مشغلات GitHub Actions مؤقتة لمهمة واحدة ومحصورة على نطاق المستودع على DigitalOcean أو AWS EC2 أو Google Compute Engine أو أجهزة Azure الافتراضية.

## البنية الأمنية

- يتحقق من تواقيع HMAC للويب هوك (webhook)، ومعرفات التثبيت، ومعرفات التسليم، وقوائم السماح للمستودعات.
- يحفظ عمليات التسليم بشكل دائم قبل إرسال الإقرار، ويتحقق من أن المهام ما زالت في قائمة الانتظار عبر GitHub Actions API قبل توفير الموارد.
- يولد إعدادات مشغل GitHub JIT أحادية الاستخدام ومحصورة على المستودع، ويوسم كل جهاز افتراضي بملكية وحدة التحكم قبل الإنشاء.
- يحذف فقط المثيلات المملوكة لوحدة التحكم بعد انتهاء المهمة أو انتهاء صلاحية TTL.

## المزودون المدعومون

| `COMPUTE_PROVIDER` | السعة القياسية | السعة القابلة للمقاطعة |
| --- | --- | --- |
| `digitalocean` | Droplet | غير متوفرة (`RUNNER_SPOT=true` يفشل بشكل آمن) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## التوطين

يدمج الملفان التنفيذيان `webhook` و`installation-router` ترجمات لـ 45 لغة محلية (20 من المستوى 1، و15 لغة إفريقية، و10 لغات آسيوية) داخل `internal/i18n`. يحترم اختيار اللغة متغيرات `LC_ALL` و`LC_MESSAGES` و`LANG` و`--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## التطوير والترخيص

```bash
go test ./...
make build
```

راجع [ملف README الإنجليزي الكامل](../../../README.md) لتفاصيل النشر والتشغيل. مرخص بموجب [MIT](../../../LICENSE).
