> [English](../../../README.md)

# مشغّلات GitHub Actions الفورية (JIT) متعددة السحب

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

وحدة تحكّم بلغة Go تنشئ مشغّلات GitHub Actions مؤقّتةً أحادية المهمة ومحصورةً في نطاق المستودع على DigitalOcean أو AWS EC2 أو Google Compute Engine أو أجهزة Azure الافتراضية.

## البنية الأمنية

- التحقّق من توقيعات HMAC لخطّافات الويب، ومعرّفات التثبيت، ومعرّفات التسليم، وقوائم المستودعات المسموح بها.
- حفظ عمليات التسليم بشكل دائم قبل إرسال الإقرار، والتحقّق من أن المهام لا تزال في قائمة الانتظار عبر واجهة برمجة تطبيقات GitHub Actions قبل التخصيص.
- توليد إعدادات مشغّل GitHub JIT أحادية الاستخدام ومحصورةً في نطاق المستودع، ووسم كل جهاز افتراضي بملكية وحدة التحكّم قبل إنشائه.
- حذف المثيلات المملوكة لوحدة التحكّم فقط بعد اكتمال المهمة أو انتهاء مدّة الصلاحية (TTL).

## المزوّدون المدعومون

| `COMPUTE_PROVIDER` | السعة القياسية | السعة القابلة للمقاطعة |
| --- | --- | --- |
| `digitalocean` | Droplet | غير متوفّرة (`RUNNER_SPOT=true` يفشل على نحوٍ مغلق) |
| `aws` | EC2 On-Demand | EC2 Spot لمرّة واحدة |
| `gcp` | جهاز افتراضي قياسي | جهاز Spot افتراضي |
| `azure` | جهاز افتراضي قياسي | جهاز Spot افتراضي |

## التوطين

تُضمِّن الملفات التنفيذية `webhook` و`installation-router` ترجمات لـ 45 لغةً وإعدادًا محليًا (20 لغةً من المستوى الأول، و15 لغةً أفريقيةً، و10 لغاتٍ آسيويةٍ) في `internal/i18n`. يحترم اختيار اللغة متغيّرات `LC_ALL` و`LC_MESSAGES` و`LANG` و`--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## التطوير والترخيص

```bash
go test ./...
make build
```

راجع [ملف README الإنجليزي الكامل](../../../README.md) للاطّلاع على تفاصيل النشر والتشغيل. مرخّص بموجب [MIT](../../../LICENSE).
