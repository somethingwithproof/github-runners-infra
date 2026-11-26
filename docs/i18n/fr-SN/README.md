> [English](../../../README.md)

# Exécuteurs GitHub Actions JIT multicloud

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licence](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Un contrôleur Go qui crée des exécuteurs GitHub Actions éphémères, dédiés à une seule tâche et limités à un dépôt, sur des machines virtuelles DigitalOcean, AWS EC2, Google Compute Engine ou Azure.

## Architecture de sécurité

- Vérifie les signatures HMAC des webhooks, les identifiants d'installation, les identifiants de livraison et les listes d'autorisation des dépôts.
- Enregistre les livraisons de manière durable avant l'accusé de réception et vérifie via l'API GitHub Actions que les tâches sont toujours en file d'attente avant le provisionnement.
- Génère des configurations d'exécuteurs JIT GitHub à usage unique et limitées au dépôt, et étiquette chaque VM avec la propriété du contrôleur avant sa création.
- Supprime uniquement les instances appartenant au contrôleur une fois la tâche terminée ou le TTL expiré.

## Fournisseurs pris en charge

| `COMPUTE_PROVIDER` | Capacité standard | Capacité interruptible |
| --- | --- | --- |
| `digitalocean` | Droplet | Non disponible (`RUNNER_SPOT=true` échoue par défaut de manière sécurisée) |
| `aws` | EC2 On-Demand | EC2 Spot ponctuel |
| `gcp` | VM standard | VM Spot |
| `azure` | VM standard | VM Spot |

## Localisation

Les binaires `webhook` et `installation-router` intègrent des traductions pour 45 paramètres régionaux (20 de niveau 1, 15 africains et 10 asiatiques) dans `internal/i18n`. La sélection du paramètre régional respecte `LC_ALL`, `LC_MESSAGES`, `LANG` et `--lang` :

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Développement et licence

```bash
go test ./...
make build
```

Consultez le [README complet en anglais](../../../README.md) pour les détails de déploiement et d'exploitation. Sous licence [MIT](../../../LICENSE).
