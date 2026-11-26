> [English](../../../README.md)

# Runners JIT GitHub Actions multi-cloud

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![Licence](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Un contrôleur Go qui crée des runners GitHub Actions éphémères à tâche unique et limités à un dépôt sur DigitalOcean, AWS EC2, Google Compute Engine ou des machines virtuelles Azure.

## Architecture de sécurité

- Vérifie les signatures HMAC des webhooks, les identifiants d'installation, les identifiants de livraison et les listes d'autorisation de dépôts.
- Enregistre les livraisons de manière durable avant l'acquittement et vérifie via l'API GitHub Actions que les jobs sont toujours en file d'attente avant le provisionnement.
- Génère des configurations de runner JIT GitHub à usage unique et limitées au dépôt, et étiquette chaque VM avec l'appartenance au contrôleur avant sa création.
- Supprime uniquement les instances appartenant au contrôleur après la fin du job ou l'expiration du TTL.

## Fournisseurs pris en charge

| `COMPUTE_PROVIDER` | Capacité standard | Capacité interruptible |
| --- | --- | --- |
| `digitalocean` | Droplet | Indisponible (`RUNNER_SPOT=true` échoue en mode fermé) |
| `aws` | EC2 On-Demand | EC2 Spot ponctuelle |
| `gcp` | VM standard | VM Spot |
| `azure` | VM standard | VM Spot |

## Localisation

Les binaires `webhook` et `installation-router` embarquent des traductions pour 45 locales (20 de niveau 1, 15 africaines et 10 asiatiques) dans `internal/i18n`. La sélection de la locale respecte `LC_ALL`, `LC_MESSAGES`, `LANG` et `--lang` :

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Développement et licence

```bash
go test ./...
make build
```

Consultez le [README anglais](../../../README.md) complet pour les détails de déploiement et d'exploitation. Distribué sous licence [MIT](../../../LICENSE).
