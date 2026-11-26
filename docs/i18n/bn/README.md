> [English](../../../README.md)

# মাল্টি-ক্লাউড GitHub Actions JIT রানার

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

একটি Go কন্ট্রোলার যা DigitalOcean, AWS EC2, Google Compute Engine অথবা Azure VM-এ একক-জব, রিপোজিটরি-সীমাবদ্ধ ক্ষণস্থায়ী GitHub Actions রানার তৈরি করে।

## নিরাপত্তা স্থাপত্য

- ওয়েবহুক HMAC স্বাক্ষর, ইনস্টলেশন ID, ডেলিভারি ID এবং রিপোজিটরি অনুমোদিত তালিকা যাচাই করে।
- স্বীকৃতি দেওয়ার আগে ডেলিভারিগুলো স্থায়ীভাবে সংরক্ষণ করে এবং প্রভিশনিংয়ের আগে GitHub Actions API-এর মাধ্যমে জবগুলো তখনো কিউতে রয়েছে কিনা তা যাচাই করে।
- একবার ব্যবহারযোগ্য রিপোজিটরি-সীমাবদ্ধ GitHub JIT রানার কনফিগারেশন তৈরি করে এবং তৈরির আগে প্রতিটি VM-এ কন্ট্রোলার মালিকানার ট্যাগ যুক্ত করে।
- জব সম্পন্ন হওয়ার পর বা TTL মেয়াদ শেষ হলে শুধুমাত্র কন্ট্রোলারের মালিকানাধীন ইনস্ট্যান্সগুলো মুছে ফেলে।

## সমর্থিত প্রদানকারী

| `COMPUTE_PROVIDER` | স্ট্যান্ডার্ড সক্ষমতা | বিঘ্নযোগ্য সক্ষমতা |
| --- | --- | --- |
| `digitalocean` | Droplet | উপলব্ধ নয় (`RUNNER_SPOT=true` দিলে নিরাপদে ব্যর্থ হয়) |
| `aws` | EC2 On-Demand | EC2 ওয়ান-টাইম Spot |
| `gcp` | স্ট্যান্ডার্ড VM | Spot VM |
| `azure` | স্ট্যান্ডার্ড VM | Spot VM |

## স্থানীয়করণ

`webhook` এবং `installation-router` বাইনারিগুলো `internal/i18n`-এ ৪৫টি লোকেলের (২০টি Tier 1, ১৫টি আফ্রিকান এবং ১০টি এশীয় লোকেল) অনুবাদ এম্বেড করে। লোকেল নির্বাচন `LC_ALL`, `LC_MESSAGES`, `LANG` এবং `--lang` অনুসরণ করে:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## উন্নয়ন ও লাইসেন্স

```bash
go test ./...
make build
```

ডিপ্লয়মেন্ট এবং পরিচালনার বিস্তারিত তথ্যের জন্য সম্পূর্ণ [ইংরেজি README](../../../README.md) দেখুন। [MIT](../../../LICENSE) লাইসেন্সের অধীনে লাইসেন্সকৃত।
