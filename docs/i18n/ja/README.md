> [English](../../../README.md)

# マルチクラウド GitHub Actions JIT ランナー

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

DigitalOcean、AWS EC2、Google Compute Engine、または Azure VM 上に、単一ジョブ・リポジトリスコープの一時的な GitHub Actions ランナーを作成する Go 製コントローラーです。

## セキュリティアーキテクチャ

- Webhook の HMAC 署名、インストール ID、配信 ID、およびリポジトリ許可リストを検証します。
- 応答（acknowledgement）を返す前に配信内容を永続化し、プロビジョニング前に GitHub Actions API を通じてジョブがまだキューに入っていることを確認します。
- 1 回限りのリポジトリスコープ GitHub JIT ランナー設定を生成し、作成前にすべての VM へコントローラー所有タグを付与します。
- ジョブ完了後または TTL 有効期限切れ後に、コントローラーが所有するインスタンスのみを削除します。

## 対応プロバイダー

| `COMPUTE_PROVIDER` | 標準キャパシティ | 中断可能キャパシティ |
| --- | --- | --- |
| `digitalocean` | Droplet | 利用不可（`RUNNER_SPOT=true` はフェイルクローズ） |
| `aws` | EC2 オンデマンド | EC2 1 回限りスポット |
| `gcp` | 標準 VM | スポット VM |
| `azure` | 標準 VM | スポット VM |

## ローカライズ

`webhook` および `installation-router` バイナリには、45 ロケール（Tier 1 が 20、アフリカ地域が 15、アジア地域が 10）の翻訳が `internal/i18n` に埋め込まれています。ロケール選択は `LC_ALL`、`LC_MESSAGES`、`LANG`、および `--lang` を参照します。

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## 開発とライセンス

```bash
go test ./...
make build
```

デプロイおよび運用の詳細は[英語版 README](../../../README.md)を参照してください。ライセンスは [MIT](../../../LICENSE) です。
