> [English](../../../README.md)

# मल्टी-क्लाउड GitHub Actions JIT रनर्स

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![परवाना](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

DigitalOcean, AWS EC2, Google Compute Engine किंवा Azure VMs वर सिंगल-जॉब, रिपॉझिटरी-मर्यादित तात्पुरते GitHub Actions रनर्स तयार करणारा Go कंट्रोलर.

## सुरक्षा आर्किटेक्चर

- वेबहुक HMAC स्वाक्षऱ्या, इंस्टॉलेशन IDs, डिलिव्हरी IDs आणि रिपॉझिटरी अलाउलिस्टची पडताळणी करते.
- पावती देण्यापूर्वी डिलिव्हरी टिकाऊ स्वरूपात जतन करते आणि तरतूद करण्यापूर्वी GitHub Actions API द्वारे जॉब्स अद्याप रांगेत असल्याची खात्री करते.
- एकदाच वापरण्यायोग्य रिपॉझिटरी-मर्यादित GitHub JIT रनर कॉन्फिगरेशन्स तयार करते आणि निर्मितीपूर्वी प्रत्येक VM ला कंट्रोलर मालकीचा टॅग लावते.
- जॉब पूर्ण झाल्यावर किंवा TTL मुदत संपल्यावर केवळ कंट्रोलरच्या मालकीचे इन्स्टन्सेस हटवते.

## समर्थित प्रदाते

| `COMPUTE_PROVIDER` | मानक क्षमता | व्यत्ययक्षम क्षमता |
| --- | --- | --- |
| `digitalocean` | Droplet | उपलब्ध नाही (`RUNNER_SPOT=true` सुरक्षितपणे नाकारले जाते) |
| `aws` | EC2 On-Demand | EC2 वन-टाइम Spot |
| `gcp` | मानक VM | Spot VM |
| `azure` | मानक VM | Spot VM |

## स्थानिकीकरण

`webhook` आणि `installation-router` बायनरीजमध्ये `internal/i18n` अंतर्गत 45 लोकॅल्सचे (20 टियर 1, 15 आफ्रिकन आणि 10 आशियाई लोकॅल्स) भाषांतर समाविष्ट आहे. लोकॅल निवड `LC_ALL`, `LC_MESSAGES`, `LANG` आणि `--lang` चे पालन करते:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## विकास आणि परवाना

```bash
go test ./...
make build
```

तैनाती आणि संचालन तपशिलांसाठी संपूर्ण [इंग्रजी README](../../../README.md) पहा. [MIT](../../../LICENSE) अंतर्गत परवानाकृत.
