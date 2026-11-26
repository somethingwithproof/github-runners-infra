> [English](../../../README.md)

# 多云 GitHub Actions JIT Runner

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![许可证](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

一个 Go 控制器，用于在 DigitalOcean、AWS EC2、Google Compute Engine 或 Azure 虚拟机上创建单作业、仓库作用域的临时 GitHub Actions Runner。

## 安全架构

- 校验 Webhook HMAC 签名、安装 ID、投递 ID 和仓库白名单。
- 在确认响应前持久化保存投递记录，并在预配前通过 GitHub Actions API 验证作业是否仍处于排队状态。
- 生成单次使用、仓库作用域的 GitHub JIT Runner 配置，并在创建前为每台虚拟机标记控制器归属标签。
- 仅在作业完成或 TTL 过期后删除由控制器创建的实例。

## 支持的云提供商

| `COMPUTE_PROVIDER` | 标准容量 | 可中断容量 |
| --- | --- | --- |
| `digitalocean` | Droplet | 不可用（`RUNNER_SPOT=true` 将采用故障关闭策略拒绝启动） |
| `aws` | EC2 按需实例 | EC2 一次性竞价实例 |
| `gcp` | 标准虚拟机 | 抢占式虚拟机（Spot VM） |
| `azure` | 标准虚拟机 | 竞价虚拟机（Spot VM） |

## 本地化

`webhook` 和 `installation-router` 二进制文件在 `internal/i18n` 中内置了 45 个语言环境的翻译（20 个 Tier 1、15 个非洲和 10 个亚洲语言环境）。语言环境选择遵循 `LC_ALL`、`LC_MESSAGES`、`LANG` 以及 `--lang`：

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## 开发与许可证

```bash
go test ./...
make build
```

有关部署和运维详情，请参阅完整的[英文 README](../../../README.md)。基于 [MIT](../../../LICENSE) 协议开源。
