> [English](../../../README.md)

# Runners JIT Multi-cloud do GitHub Actions

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Um controlador em Go que cria runners efêmeros do GitHub Actions para um único job e com escopo de repositório na DigitalOcean, AWS EC2, Google Compute Engine ou VMs do Azure.

## Arquitetura de segurança

- Verifica assinaturas HMAC de webhooks, IDs de instalação, IDs de entrega e listas de repositórios permitidos.
- Persiste entregas de forma durável antes da confirmação e verifica se os jobs ainda estão na fila via API do GitHub Actions antes do provisionamento.
- Gera configurações de runner JIT do GitHub de uso único com escopo de repositório e marca cada VM com a propriedade do controlador antes da criação.
- Exclui apenas instâncias pertencentes ao controlador após a conclusão do job ou a expiração do TTL.

## Provedores suportados

| `COMPUTE_PROVIDER` | Capacidade padrão | Capacidade interrompível |
| --- | --- | --- |
| `digitalocean` | Droplet | Não disponível (`RUNNER_SPOT=true` falha de forma segura) |
| `aws` | EC2 On-Demand | EC2 Spot de uso único |
| `gcp` | VM padrão | VM Spot |
| `azure` | VM padrão | VM Spot |

## Localização

Os binários `webhook` e `installation-router` incorporam traduções para 45 locales (20 Tier 1, 15 africanos e 10 asiáticos) em `internal/i18n`. A seleção de locale respeita `LC_ALL`, `LC_MESSAGES`, `LANG` e `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Desenvolvimento e Licença

```bash
go test ./...
make build
```

Consulte o [README em inglês](../../../README.md) completo para detalhes de implantação e operação. Licenciado sob [MIT](../../../LICENSE).
