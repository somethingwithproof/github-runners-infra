package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	DropletsCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_runner_droplets_created_total",
		Help: "Total runner droplets created.",
	})

	DropletsDestroyed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "github_runner_droplets_destroyed_total",
		Help: "Total runner droplets destroyed.",
	}, []string{"method"})

	ProvisioningDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "github_runner_provisioning_duration_seconds",
		Help:    "Time to provision a runner droplet.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})

	ProvisioningErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_runner_provisioning_errors_total",
		Help: "Total provisioning failures.",
	})

	WebhookRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "github_runner_webhook_requests_total",
		Help: "Total webhook requests by outcome.",
	}, []string{"status"})

	WorkerPoolSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "github_runner_worker_pool_in_use",
		Help: "Number of worker pool slots currently in use.",
	})

	RateLimitHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_runner_rate_limit_hits_total",
		Help: "Total rate limit rejections.",
	})

	ActiveDroplets = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "github_runner_active_droplets",
		Help: "Number of active runner droplets (updated during cleanup).",
	})

	CleanupDeleted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_runner_cleanup_deleted_total",
		Help: "Total droplets deleted by cleanup.",
	})

	DuplicateWebhooks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_runner_duplicate_webhooks_total",
		Help: "Total duplicate webhook events suppressed.",
	})
)
