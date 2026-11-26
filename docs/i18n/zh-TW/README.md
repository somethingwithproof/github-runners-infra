> [English](../../../README.md)

# 多雲端 GitHub Actions JIT Runners

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

一個 Go 控制器，可在 DigitalOcean、AWS EC2、Google Compute Engine 或 Azure VM 上建立單次作業、限於單一儲存庫範圍的暫時性 GitHub Actions Runner。

## 安全架構

- 驗證 Webhook HMAC 簽章、安裝 ID、傳遞 ID 與儲存庫允許清單。
- 在確認接收前將傳遞記錄持久化儲存，並在佈建前透過 GitHub Actions API 驗證作業是否仍處於佇列中。
- 產生單次使用、限於單一儲存庫範圍的 GitHub JIT Runner 設定，並在建立前為每個 VM 標記控制器擁有權標籤。
- 僅在作業完成或 TTL 到期後刪除由控制器擁有的執行個體。

## 支援的雲端供應商

| `COMPUTE_PROVIDER` | 標準容量 | 可中斷容量 |
| --- | --- | --- |
| `digitalocean` | Droplet | 無法使用（`RUNNER_SPOT=true` 會採失效關閉） |
| `aws` | EC2 On-Demand | EC2 單次 Spot |
| `gcp` | 標準 VM | Spot VM |
| `azure` | 標準 VM | Spot VM |

## 在地化

`webhook` 與 `installation-router` 執行檔在 `internal/i18n` 中內嵌了 45 種語系的翻譯（包含 20 種 Tier 1 語系、15 種非洲語系與 10 種亞洲語系）。語系選取會依循 `LC_ALL`、`LC_MESSAGES`、`LANG` 與 `--lang`：

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## 開發與授權條款

```bash
go test ./...
make build
```

部署與維運詳細資訊請參閱完整的[英文 README](../../../README.md)。採用 [MIT](../../../LICENSE) 授權條款。
