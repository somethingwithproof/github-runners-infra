// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package budget enforces fail-closed daily, monthly, and per-repository USD
// spend limits for runner provisioning while never blocking runner cleanup.
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultReservationDuration = 15 * time.Minute
	minAccruedDuration         = time.Minute
	costEpsilon                = 1e-9
)

// ErrBudgetExceeded indicates that provisioning a new runner would exceed a
// configured daily, monthly, or per-repository spend cap.
var ErrBudgetExceeded = errors.New("spend budget cap exceeded")

// Config configures hard spend limits and optional durable ledger persistence.
type Config struct {
	DailyCapUSD         float64
	MonthlyCapUSD       float64
	RepoDailyCapUSD     float64
	ReservationDuration time.Duration
	StatePath           string
	Now                 func() time.Time
}

// Reservation describes a runner about to be provisioned.
type Reservation struct {
	Key           string
	Repository    string
	Provider      string
	Size          string
	HourlyRateUSD float64
}

// Metrics summarizes current spend and circuit-breaker counters for observability.
type Metrics struct {
	Day                string             `json:"day"`
	Month              string             `json:"month"`
	DailySpendUSD      float64            `json:"daily_spend_usd"`
	MonthlySpendUSD    float64            `json:"monthly_spend_usd"`
	InFlightSpendUSD   float64            `json:"in_flight_spend_usd"`
	RepoDailySpendUSD  map[string]float64 `json:"repo_daily_spend_usd"`
	BudgetBlockedTotal uint64             `json:"budget_blocked_total"`
}

type activeReservation struct {
	Repository      string    `json:"repository"`
	Provider        string    `json:"provider"`
	Size            string    `json:"size"`
	HourlyRateUSD   float64   `json:"hourly_rate_usd"`
	EstimatedCapUSD float64   `json:"estimated_cap_usd"`
	StartedAt       time.Time `json:"started_at"`
}

type ledgerState struct {
	Day                string                       `json:"day"`
	Month              string                       `json:"month"`
	DailyAccruedUSD    float64                      `json:"daily_accrued_usd"`
	MonthlyAccruedUSD  float64                      `json:"monthly_accrued_usd"`
	RepoDailyAccrued   map[string]float64           `json:"repo_daily_accrued"`
	InFlight           map[string]activeReservation `json:"in_flight"`
	BudgetBlockedTotal uint64                       `json:"budget_blocked_total"`
}

// Tracker tracks accrued and in-flight runner costs and enforces fail-closed
// spend caps. All methods are safe for concurrent use.
type Tracker struct {
	mu                  sync.Mutex
	dailyCapUSD         float64
	monthlyCapUSD       float64
	repoDailyCapUSD     float64
	reservationDuration time.Duration
	statePath           string
	now                 func() time.Time
	state               ledgerState
}

// NewTracker constructs a Tracker and restores persisted spend state when
// StatePath exists.
func NewTracker(cfg Config) (*Tracker, error) {
	for name, val := range map[string]float64{
		"BUDGET_DAILY_USD":      cfg.DailyCapUSD,
		"BUDGET_MONTHLY_USD":    cfg.MonthlyCapUSD,
		"BUDGET_REPO_DAILY_USD": cfg.RepoDailyCapUSD,
	} {
		if math.IsNaN(val) || math.IsInf(val, 0) || val < 0 {
			return nil, fmt.Errorf("%s must be a non-negative finite number", name)
		}
	}
	resDuration := cfg.ReservationDuration
	if resDuration <= 0 {
		resDuration = defaultReservationDuration
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	nowUTC := nowFn().UTC()
	t := &Tracker{
		dailyCapUSD:         cfg.DailyCapUSD,
		monthlyCapUSD:       cfg.MonthlyCapUSD,
		repoDailyCapUSD:     cfg.RepoDailyCapUSD,
		reservationDuration: resDuration,
		statePath:           strings.TrimSpace(cfg.StatePath),
		now:                 nowFn,
		state: ledgerState{
			Day:              nowUTC.Format("2006-01-02"),
			Month:            nowUTC.Format("2006-01"),
			RepoDailyAccrued: make(map[string]float64),
			InFlight:         make(map[string]activeReservation),
		},
	}

	if t.statePath != "" {
		if err := t.loadLocked(); err != nil {
			return nil, err
		}
		t.rolloverLocked(nowUTC)
	}
	return t, nil
}

// Enabled reports whether at least one spend cap is configured.
func (t *Tracker) Enabled() bool {
	if t == nil {
		return false
	}
	return t.dailyCapUSD > 0 || t.monthlyCapUSD > 0 || t.repoDailyCapUSD > 0
}

// Reserve checks configured caps against accrued + in-flight + estimated new
// runner cost. When allowed, it records an in-flight reservation; when blocked,
// it increments budget_blocked_total and returns ErrBudgetExceeded.
func (t *Tracker) Reserve(res Reservation) error {
	if t == nil {
		return nil
	}
	if res.Key == "" {
		return fmt.Errorf("budget reservation requires a non-empty job key")
	}
	if math.IsNaN(res.HourlyRateUSD) || math.IsInf(res.HourlyRateUSD, 0) || res.HourlyRateUSD < 0 {
		return fmt.Errorf("budget reservation requires a non-negative hourly rate")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	nowUTC := t.now().UTC()
	t.rolloverLocked(nowUTC)

	if _, exists := t.state.InFlight[res.Key]; exists {
		return nil
	}

	repo := strings.ToLower(strings.TrimSpace(res.Repository))
	estCost := res.HourlyRateUSD * t.reservationDuration.Hours()
	inFlightTotal, inFlightRepo := t.inFlightTotalsLocked(nowUTC, repo)

	projectedDaily := t.state.DailyAccruedUSD + inFlightTotal + estCost
	projectedMonthly := t.state.MonthlyAccruedUSD + inFlightTotal + estCost
	projectedRepoDaily := t.state.RepoDailyAccrued[repo] + inFlightRepo + estCost

	if t.dailyCapUSD > 0 && projectedDaily > t.dailyCapUSD+costEpsilon {
		return t.blockLocked("daily", repo, projectedDaily, t.dailyCapUSD)
	}
	if t.monthlyCapUSD > 0 && projectedMonthly > t.monthlyCapUSD+costEpsilon {
		return t.blockLocked("monthly", repo, projectedMonthly, t.monthlyCapUSD)
	}
	if t.repoDailyCapUSD > 0 && repo != "" && projectedRepoDaily > t.repoDailyCapUSD+costEpsilon {
		return t.blockLocked("repo_daily", repo, projectedRepoDaily, t.repoDailyCapUSD)
	}

	t.state.InFlight[res.Key] = activeReservation{
		Repository:      repo,
		Provider:        strings.ToLower(strings.TrimSpace(res.Provider)),
		Size:            strings.TrimSpace(res.Size),
		HourlyRateUSD:   res.HourlyRateUSD,
		EstimatedCapUSD: estCost,
		StartedAt:       nowUTC,
	}
	if err := t.persistLocked(); err != nil {
		delete(t.state.InFlight, res.Key)
		return err
	}
	return nil
}

// Release settles an in-flight runner into accrued daily/monthly spend based on
// elapsed runtime. Release is never blocked by budget caps.
func (t *Tracker) Release(key string) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	nowUTC := t.now().UTC()
	t.rolloverLocked(nowUTC)

	active, ok := t.state.InFlight[key]
	if !ok {
		return
	}
	delete(t.state.InFlight, key)

	elapsed := nowUTC.Sub(active.StartedAt)
	if elapsed < minAccruedDuration {
		elapsed = minAccruedDuration
	}
	actualCost := active.HourlyRateUSD * elapsed.Hours()
	t.state.DailyAccruedUSD += actualCost
	t.state.MonthlyAccruedUSD += actualCost
	if active.Repository != "" {
		t.state.RepoDailyAccrued[active.Repository] += actualCost
	}
	if err := t.persistLocked(); err != nil {
		log.Printf("WARN: persist budget ledger after release for %s: %v", key, err)
	}
}

// Cancel drops an in-flight reservation without accruing spend (used when
// provisioning fails before an instance is launched).
func (t *Tracker) Cancel(key string) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.state.InFlight[key]; !ok {
		return
	}
	delete(t.state.InFlight, key)
	if err := t.persistLocked(); err != nil {
		log.Printf("WARN: persist budget ledger after cancel for %s: %v", key, err)
	}
}

// Snapshot returns current spend estimates and blocked counters.
func (t *Tracker) Snapshot() Metrics {
	if t == nil {
		return Metrics{RepoDailySpendUSD: map[string]float64{}}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	nowUTC := t.now().UTC()
	t.rolloverLocked(nowUTC)

	inFlightTotal, _ := t.inFlightTotalsLocked(nowUTC, "")
	repoSpend := make(map[string]float64, len(t.state.RepoDailyAccrued))
	for repo, cost := range t.state.RepoDailyAccrued {
		repoSpend[repo] = cost
	}
	for _, active := range t.state.InFlight {
		if active.Repository != "" {
			repoSpend[active.Repository] += activeEstimate(active, nowUTC)
		}
	}

	return Metrics{
		Day:                t.state.Day,
		Month:              t.state.Month,
		DailySpendUSD:      t.state.DailyAccruedUSD + inFlightTotal,
		MonthlySpendUSD:    t.state.MonthlyAccruedUSD + inFlightTotal,
		InFlightSpendUSD:   inFlightTotal,
		RepoDailySpendUSD:  repoSpend,
		BudgetBlockedTotal: t.state.BudgetBlockedTotal,
	}
}

func (t *Tracker) blockLocked(window, repo string, projected, capUSD float64) error {
	t.state.BudgetBlockedTotal++
	if err := t.persistLocked(); err != nil {
		log.Printf("WARN: persist budget block counter: %v", err)
	}
	log.Printf("WARN: budget circuit breaker blocked provisioning (window=%s repo=%s projected_usd=%.4f cap_usd=%.4f blocked_total=%d)",
		window, repo, projected, capUSD, t.state.BudgetBlockedTotal)
	return fmt.Errorf("%w: %s cap $%.2f reached (projected $%.4f)", ErrBudgetExceeded, window, capUSD, projected)
}

func (t *Tracker) rolloverLocked(nowUTC time.Time) {
	day := nowUTC.Format("2006-01-02")
	month := nowUTC.Format("2006-01")
	if t.state.Day != day {
		t.state.Day = day
		t.state.DailyAccruedUSD = 0
		t.state.RepoDailyAccrued = make(map[string]float64)
	}
	if t.state.Month != month {
		t.state.Month = month
		t.state.MonthlyAccruedUSD = 0
	}
}

func (t *Tracker) inFlightTotalsLocked(nowUTC time.Time, repo string) (total, forRepo float64) {
	for _, active := range t.state.InFlight {
		cost := activeEstimate(active, nowUTC)
		total += cost
		if repo != "" && active.Repository == repo {
			forRepo += cost
		}
	}
	return total, forRepo
}

func activeEstimate(active activeReservation, nowUTC time.Time) float64 {
	elapsedCost := active.HourlyRateUSD * max(nowUTC.Sub(active.StartedAt), 0).Hours()
	if elapsedCost > active.EstimatedCapUSD {
		return elapsedCost
	}
	return active.EstimatedCapUSD
}

func (t *Tracker) loadLocked() error {
	raw, err := os.ReadFile(t.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read budget ledger %s: %w", t.statePath, err)
	}
	var loaded ledgerState
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("decode budget ledger %s: %w", t.statePath, err)
	}
	if loaded.RepoDailyAccrued == nil {
		loaded.RepoDailyAccrued = make(map[string]float64)
	}
	if loaded.InFlight == nil {
		loaded.InFlight = make(map[string]activeReservation)
	}
	t.state = loaded
	return nil
}

func (t *Tracker) persistLocked() error {
	if t.statePath == "" {
		return nil
	}
	encoded, err := json.MarshalIndent(t.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode budget ledger: %w", err)
	}
	dir := filepath.Dir(t.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create budget ledger directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".budget-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary budget ledger: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write budget ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync budget ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close budget ledger: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod budget ledger: %w", err)
	}
	if err := os.Rename(tmpPath, t.statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("commit budget ledger: %w", err)
	}
	return nil
}
