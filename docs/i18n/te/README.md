> [English](../../../README.md)

# మల్టీ-క్లౌడ్ GitHub Actions JIT రన్నర్లు

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

DigitalOcean, AWS EC2, Google Compute Engine, లేదా Azure VM లపై ఒకే-జాబ్, రిపోజిటరీ-పరిమిత తాత్కాలిక GitHub Actions రన్నర్లను సృష్టించే Go కంట్రోలర్.

## భద్రతా నిర్మాణం

- వెబ్‌హుక్ HMAC సంతకాలు, ఇన్‌స్టాలేషన్ ID లు, డెలివరీ ID లు, మరియు రిపోజిటరీ అనుమతి జాబితాలను ధృవీకరిస్తుంది.
- నిర్ధారణకు ముందే డెలివరీలను శాశ్వతంగా భద్రపరుస్తుంది మరియు ప్రొవిజనింగ్‌కు ముందు GitHub Actions API ద్వారా జాబ్‌లు ఇంకా క్యూలో ఉన్నాయని ధృవీకరిస్తుంది.
- ఒకసారి మాత్రమే ఉపయోగించే రిపోజిటరీ-పరిమిత GitHub JIT రన్నర్ కాన్ఫిగరేషన్‌లను రూపొందిస్తుంది మరియు సృష్టించడానికి ముందే ప్రతి VM కు కంట్రోలర్ యాజమాన్య ట్యాగ్‌ను జతచేస్తుంది.
- జాబ్ పూర్తయిన తర్వాత లేదా TTL గడువు ముగిసిన తర్వాత కంట్రోలర్ యాజమాన్యంలోని ఇన్‌స్టాన్స్‌లను మాత్రమే తొలగిస్తుంది.

## మద్దతు ఉన్న ప్రొవైడర్లు

| `COMPUTE_PROVIDER` | ప్రామాణిక సామర్థ్యం | అంతరాయం కలిగే సామర్థ్యం |
| --- | --- | --- |
| `digitalocean` | Droplet | అందుబాటులో లేదు (`RUNNER_SPOT=true` సురక్షితంగా విఫలమవుతుంది) |
| `aws` | EC2 On-Demand | EC2 వన్-టైమ్ Spot |
| `gcp` | ప్రామాణిక VM | Spot VM |
| `azure` | ప్రామాణిక VM | Spot VM |

## స్థానికీకరణ

`webhook` మరియు `installation-router` బైనరీలు 45 లోకేల్స్ కోసం అనువాదాలను (20 టియర్ 1, 15 ఆఫ్రికన్, మరియు 10 ఆసియా లోకేల్స్) `internal/i18n` లో పొందుపరుస్తాయి. లోకేల్ ఎంపిక `LC_ALL`, `LC_MESSAGES`, `LANG`, మరియు `--lang` లను అనుసరిస్తుంది:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## అభివృద్ధి మరియు లైసెన్స్

```bash
go test ./...
make build
```

విస్తరణ మరియు నిర్వహణ వివరాల కోసం పూర్తి [ఆంగ్ల README](../../../README.md) ను చూడండి. [MIT](../../../LICENSE) క్రింద లైసెన్స్ పొందింది.
