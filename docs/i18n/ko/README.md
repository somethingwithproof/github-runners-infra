> [English](../../../README.md)

# 멀티 클라우드 GitHub Actions JIT 러너

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

DigitalOcean, AWS EC2, Google Compute Engine 또는 Azure VM에서 단일 작업(single-job), 리포지토리 범위의 임시 GitHub Actions 러너를 생성하는 Go 컨트롤러입니다.

## 보안 아키텍처

- 웹훅 HMAC 서명, 설치 ID, 전송 ID 및 리포지토리 허용 목록을 검증합니다.
- 응답(acknowledgement) 전에 전송 내역을 영구 저장하고, 프로비저닝 전에 GitHub Actions API를 통해 작업이 여전히 대기열에 있는지 확인합니다.
- 단일 사용 리포지토리 범위 GitHub JIT 러너 구성을 생성하고, 생성 전에 모든 VM에 컨트롤러 소유권 태그를 지정합니다.
- 작업 완료 또는 TTL 만료 후 컨트롤러가 소유한 인스턴스만 삭제합니다.

## 지원 프로바이더

| `COMPUTE_PROVIDER` | 표준 용량 | 선점형(인터럽트 가능) 용량 |
| --- | --- | --- |
| `digitalocean` | Droplet | 지원 안 함 (`RUNNER_SPOT=true` 시 fail-closed로 실패) |
| `aws` | EC2 온디맨드 | EC2 일회성 스팟 |
| `gcp` | 표준 VM | 스팟 VM |
| `azure` | 표준 VM | 스팟 VM |

## 현지화

`webhook` 및 `installation-router` 바이너리는 `internal/i18n`에 45개 로캘(Tier 1 20개, 아프리카 15개, 아시아 10개)의 번역을 내장하고 있습니다. 로캘 선택은 `LC_ALL`, `LC_MESSAGES`, `LANG` 및 `--lang`을 따릅니다:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## 개발 및 라이선스

```bash
go test ./...
make build
```

배포 및 운영 세부 정보는 전체 [영문 README](../../../README.md)를 참조하세요. [MIT](../../../LICENSE)에 따라 라이선스가 부여됩니다.
