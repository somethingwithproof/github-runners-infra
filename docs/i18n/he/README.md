> [English](../../../README.md)

# רצי JIT של GitHub Actions בריבוי עננים

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![רישיון](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

בקר Go היוצר רצי GitHub Actions ארעיים למשימה בודדת וברמת מאגר על גבי מכונות וירטואליות של DigitalOcean, AWS EC2, Google Compute Engine או Azure.

## ארכיטקטורת אבטחה

- מאמת חתימות HMAC של webhook, מזהי התקנה (installation IDs), מזהי מסירה (delivery IDs) ורשימות היתר של מאגרים.
- שומר מסירות באופן עמיד לפני אישור קבלה ומאמת שמשימות עדיין ממתינות בתור דרך ה-API של GitHub Actions לפני הקצאת משאבים.
- מפיק תצורות רץ JIT של GitHub לשימוש חד-פעמי ברמת מאגר ומתייג כל מכונה וירטואלית בבעלות הבקר לפני יצירתה.
- מוחק אך ורק מופעים בבעלות הבקר לאחר השלמת המשימה או פקיעת ה-TTL.

## ספקים נתמכים

| `COMPUTE_PROVIDER` | קיבולת רגילה | קיבולת ניתנת להפסקה |
| --- | --- | --- |
| `digitalocean` | Droplet | לא זמין (`RUNNER_SPOT=true` נכשל באופן סגור) |
| `aws` | EC2 On-Demand | EC2 Spot חד-פעמי |
| `gcp` | VM רגילה | Spot VM |
| `azure` | VM רגילה | Spot VM |

## לוקליזציה

הקבצים הבינאריים `webhook` ו-`installation-router` מטמיעים תרגומים עבור 45 אזורי שפה (20 אזורי Tier 1, 15 אזורים אפריקאיים ו-10 אזורים אסייתיים) תחת `internal/i18n`. בחירת אזור השפה מתחשבת במשתנים `LC_ALL`, `LC_MESSAGES`, `LANG` וב-`--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## פיתוח ורישיון

```bash
go test ./...
make build
```

ראו את קובץ ה-[README המלא באנגלית](../../../README.md) לפרטי פריסה ותפעול. מורשה תחת [MIT](../../../LICENSE).
