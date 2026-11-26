> [English](../../../README.md)

# मल्टी-क्लाउड GitHub Actions JIT रनर

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

एक Go नियंत्रक जो DigitalOcean, AWS EC2, Google Compute Engine या Azure VM पर एकल-जॉब, रिपॉज़िटरी-स्कोप वाले अस्थायी GitHub Actions रनर बनाता है।

## सुरक्षा वास्तुकला

- वेबहुक HMAC हस्ताक्षर, इंस्टॉलेशन आईडी, डिलीवरी आईडी और रिपॉज़िटरी अनुमति-सूचियों की पुष्टि करता है।
- पावती से पहले डिलीवरी को स्थायी रूप से सहेजता है और प्रोविज़निंग से पहले GitHub Actions API के माध्यम से पुष्टि करता है कि जॉब अभी भी कतार में हैं।
- एकल-उपयोग रिपॉज़िटरी-स्कोप वाले GitHub JIT रनर कॉन्फ़िगरेशन तैयार करता है और निर्माण से पहले प्रत्येक VM को नियंत्रक स्वामित्व के साथ टैग करता है।
- जॉब पूरा होने या TTL समाप्त होने के बाद केवल नियंत्रक के स्वामित्व वाले इंस्टेंस हटाता है।

## समर्थित प्रदाता

| `COMPUTE_PROVIDER` | मानक क्षमता | बाधित होने योग्य क्षमता |
| --- | --- | --- |
| `digitalocean` | Droplet | उपलब्ध नहीं (`RUNNER_SPOT=true` सुरक्षित रूप से विफल होता है) |
| `aws` | EC2 On-Demand | EC2 वन-टाइम Spot |
| `gcp` | मानक VM | Spot VM |
| `azure` | मानक VM | Spot VM |

## स्थानीयकरण

`webhook` और `installation-router` बाइनरी `internal/i18n` में 45 लोकेल (20 टियर 1, 15 अफ़्रीकी और 10 एशियाई लोकेल) के लिए अनुवाद एम्बेड करते हैं। लोकेल चयन `LC_ALL`, `LC_MESSAGES`, `LANG` और `--lang` का सम्मान करता है:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## विकास और लाइसेंस

```bash
go test ./...
make build
```

परिनियोजन और संचालन विवरण के लिए पूर्ण [अंग्रेज़ी README](../../../README.md) देखें। [MIT](../../../LICENSE) के अंतर्गत लाइसेंस प्राप्त।
