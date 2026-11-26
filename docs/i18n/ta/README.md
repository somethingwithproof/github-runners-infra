> [English](../../../README.md)

# பல-மேகக்கணிமை GitHub Actions JIT இயக்கிகள்

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

DigitalOcean, AWS EC2, Google Compute Engine அல்லது Azure VM களில் ஒற்றைப் பணி, களஞ்சிய வரம்பிற்குட்பட்ட தற்காலிக GitHub Actions இயக்கிகளை உருவாக்கும் Go கட்டுப்படுத்தி.

## பாதுகாப்பு கட்டமைப்பு

- webhook HMAC கையொப்பங்கள், நிறுவல் அடையாள எண்கள், விநியோக அடையாள எண்கள் மற்றும் களஞ்சிய அனுமதிப் பட்டியல்களைச் சரிபார்க்கிறது.
- ஒப்புகைக்கு முன் விநியோகங்களை நிலைத்திருப்பதாகச் சேமிக்கிறது மற்றும் வழங்குவதற்கு முன் GitHub Actions API வழியாகப் பணிகள் இன்னும் வரிசையில் உள்ளதா என்பதைச் சரிபார்க்கிறது.
- ஒருமுறை பயன்படுத்தும் களஞ்சிய வரம்பிற்குட்பட்ட GitHub JIT இயக்கி உள்ளமைவுகளை உருவாக்கி, உருவாக்குவதற்கு முன் ஒவ்வொரு VM க்கும் கட்டுப்படுத்தி உரிமையாளர் குறிச்சொல்லை இடுகிறது.
- பணி முடிந்த பிறகு அல்லது TTL காலாவதியான பிறகு கட்டுப்படுத்திக்குச் சொந்தமான நிகழ்வுகளை மட்டுமே நீக்குகிறது.

## ஆதரிக்கப்படும் வழங்குநர்கள்

| `COMPUTE_PROVIDER` | நிலையான திறன் | குறுக்கிடக்கூடிய திறன் |
| --- | --- | --- |
| `digitalocean` | Droplet | கிடைக்காது (`RUNNER_SPOT=true` பாதுகாப்பாகத் தோல்வியடையும்) |
| `aws` | EC2 On-Demand | EC2 ஒருமுறை Spot |
| `gcp` | நிலையான VM | Spot VM |
| `azure` | நிலையான VM | Spot VM |

## உள்ளூர்மயமாக்கல்

`webhook` மற்றும் `installation-router` இருமங்கள் `internal/i18n` இல் 45 மொழியிடங்களுக்கான (20 Tier 1, 15 ஆப்பிரிக்க மற்றும் 10 ஆசிய மொழியிடங்கள்) மொழிபெயர்ப்புகளை உட்பொதித்துள்ளன. மொழியிடத் தேர்வு `LC_ALL`, `LC_MESSAGES`, `LANG` மற்றும் `--lang` ஆகியவற்றைப் பின்பற்றுகிறது:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## மேம்பாடு மற்றும் உரிமம்

```bash
go test ./...
make build
```

பயன்பாடு மற்றும் செயல்பாட்டு விவரங்களுக்கு முழுமையான [ஆங்கில README](../../../README.md) ஐப் பார்க்கவும். [MIT](../../../LICENSE) உரிமத்தின் கீழ் வெளியிடப்பட்டது.
