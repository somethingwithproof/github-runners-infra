> [English](../../../README.md)

# ባለብዙ-ክላውድ የGitHub Actions JIT አስፈጻሚዎች

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![ፈቃድ](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

በDigitalOcean፣ AWS EC2፣ Google Compute Engine ወይም Azure VM ላይ ለአንድ ስራ ብቻ የሚያገለግሉ እና በማከማቻ የተገደቡ ጊዜያዊ የGitHub Actions አስፈጻሚዎችን የሚፈጥር የGo ተቆጣጣሪ።

## የደህንነት አርክቴክቸር

- የwebhook HMAC ፊርማዎችን፣ የመጫኛ መለያዎችን፣ የማድረሻ መለያዎችን እና የማከማቻ ፈቃድ ዝርዝሮችን ያረጋግጣል።
- ማረጋገጫ ከመስጠቱ በፊት ማድረሻዎችን በዘላቂነት ያስቀምጣል እና አቅርቦት ከመጀመሩ በፊት ስራዎች አሁንም በሰልፍ ላይ መሆናቸውን በGitHub Actions API በኩል ያረጋግጣል።
- ለአንድ ጊዜ አገልግሎት የሚውሉ እና በማከማቻ የተገደቡ የGitHub JIT አስፈጻሚ ውቅሮችን ያመነጫል እንዲሁም ከመፈጠሩ በፊት እያንዳንዱን VM በተቆጣጣሪው ባለቤትነት ምልክት ያደርጋል።
- ስራው ሲጠናቀቅ ወይም TTL ጊዜው ሲያልፍ በተቆጣጣሪው ባለቤትነት የተያዙትን አጋጣሚዎች ብቻ ይሰርዛል።

## የሚደገፉ አቅራቢዎች

| `COMPUTE_PROVIDER` | መደበኛ አቅም | ሊቋረጥ የሚችል አቅም |
| --- | --- | --- |
| `digitalocean` | Droplet | አይገኝም (`RUNNER_SPOT=true` በዝግ ሁኔታ ይከሽፋል) |
| `aws` | EC2 On-Demand | EC2 አንድ ጊዜ Spot |
| `gcp` | መደበኛ VM | Spot VM |
| `azure` | መደበኛ VM | Spot VM |

## አካባቢያዊ ትርጉም

የ`webhook` እና `installation-router` ሁለትዮሽ ፋይሎች በ`internal/i18n` ውስጥ ለ45 አካባቢዎች (20 ደረጃ 1፣ 15 የአፍሪካ እና 10 የእስያ አካባቢዎች) ትርጉሞችን ያካትታሉ። የአካባቢ ምርጫ `LC_ALL`፣ `LC_MESSAGES`፣ `LANG` እና የ`--lang` አማራጭን ያከብራል፦

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## ልማት እና ፈቃድ

```bash
go test ./...
make build
```

ለማሰማራት እና ለአሠራር ዝርዝሮች ሙሉውን [የእንግሊዝኛ README](../../../README.md) ይመልከቱ። በ[MIT](../../../LICENSE) ፈቃድ የተሰጠ።
