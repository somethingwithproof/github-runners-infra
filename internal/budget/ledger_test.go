// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package budget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDailyMonthlyAndRepoCapEnforcement(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	tracker, err := NewTracker(Config{
		DailyCapUSD:         1.00,
		MonthlyCapUSD:       2.00,
		RepoDailyCapUSD:     0.60,
		ReservationDuration: time.Hour,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.Enabled() {
		t.Fatal("expected tracker to be enabled")
	}

	// First reservation ($0.50) fits daily, monthly, and repo daily caps.
	if err := tracker.Reserve(Reservation{
		Key: "org/a:1", Repository: "org/a", Provider: "vultr", Size: "vc2-4c-8gb", HourlyRateUSD: 0.50,
	}); err != nil {
		t.Fatalf("first Reserve error = %v", err)
	}

	// Second reservation for org/a ($0.20 -> $0.70 > $0.60) hits repo daily cap.
	err = tracker.Reserve(Reservation{
		Key: "org/a:2", Repository: "org/a", Provider: "vultr", Size: "vc2-4c-8gb", HourlyRateUSD: 0.20,
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("repo cap error = %v, want ErrBudgetExceeded", err)
	}

	// Second reservation for org/b ($0.50 -> daily $1.00) fits!
	if err := tracker.Reserve(Reservation{
		Key: "org/b:1", Repository: "org/b", Provider: "linode", Size: "g6-standard-4", HourlyRateUSD: 0.50,
	}); err != nil {
		t.Fatalf("org/b Reserve error = %v", err)
	}

	// Third reservation ($0.10 -> daily $1.10 > $1.00) hits daily cap.
	err = tracker.Reserve(Reservation{
		Key: "org/c:1", Repository: "org/c", Provider: "vultr", Size: "vc2-2c-4gb", HourlyRateUSD: 0.10,
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("daily cap error = %v, want ErrBudgetExceeded", err)
	}

	metrics := tracker.Snapshot()
	if metrics.BudgetBlockedTotal != 2 {
		t.Fatalf("BudgetBlockedTotal = %d, want 2", metrics.BudgetBlockedTotal)
	}

	// Release settles actual elapsed runtime even while over cap (cleanup is never blocked).
	now = now.Add(30 * time.Minute)
	tracker.Release("org/a:1")
	tracker.Release("org/b:1")
	metrics = tracker.Snapshot()
	if metrics.InFlightSpendUSD != 0 {
		t.Fatalf("InFlightSpendUSD = %v, want 0", metrics.InFlightSpendUSD)
	}
	if metrics.DailySpendUSD < 0.49 || metrics.DailySpendUSD > 0.51 {
		t.Fatalf("DailySpendUSD after 30m release = %.4f, want ~0.50", metrics.DailySpendUSD)
	}
}

func TestUTCDayAndMonthRolloverAndPersistence(t *testing.T) {
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	ledgerPath := filepath.Join(t.TempDir(), "budget.json")

	tracker, err := NewTracker(Config{
		DailyCapUSD:         1.00,
		MonthlyCapUSD:       1.20,
		ReservationDuration: time.Hour,
		StatePath:           ledgerPath,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Reserve(Reservation{Key: "org/r:1", Repository: "org/r", HourlyRateUSD: 1.00}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour) // 2026-12-01 00:00:00Z -> next day AND next month!
	tracker.Release("org/r:1")

	// Reload from disk on the new day/month; persisted state restores cleanly.
	reloaded, err := NewTracker(Config{
		DailyCapUSD:         1.00,
		MonthlyCapUSD:       1.20,
		ReservationDuration: time.Hour,
		StatePath:           ledgerPath,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap := reloaded.Snapshot(); snap.Day != "2026-12-01" || snap.Month != "2026-12" {
		t.Fatalf("rollover snapshot = %#v", snap)
	}

	// Advance to next UTC day inside same month: daily resets, monthly persists.
	now = now.Add(24 * time.Hour)
	if snap := reloaded.Snapshot(); snap.Day != "2026-12-02" || snap.DailySpendUSD != 0 || snap.MonthlySpendUSD < 0.99 {
		t.Fatalf("next-day snapshot = %#v", snap)
	}
	if err := reloaded.Reserve(Reservation{Key: "org/r:2", Repository: "org/r", HourlyRateUSD: 0.50}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("monthly cap after day rollover = %v, want ErrBudgetExceeded", err)
	}
}

func TestConcurrentReservationsRespectCap(t *testing.T) {
	tracker, err := NewTracker(Config{
		DailyCapUSD:         0.50,
		ReservationDuration: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, blocked := 0, 0
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			err := tracker.Reserve(Reservation{
				Key:           fmt.Sprintf("org/r:%d", idx),
				Repository:    "org/r",
				HourlyRateUSD: 0.10,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				allowed++
			} else if errors.Is(err, ErrBudgetExceeded) {
				blocked++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if allowed != 5 || blocked != 15 {
		t.Fatalf("concurrent allowed=%d blocked=%d, want 5 and 15", allowed, blocked)
	}
}

func TestCancelDoesNotAccrueSpend(t *testing.T) {
	tracker, err := NewTracker(Config{
		DailyCapUSD:         1.00,
		ReservationDuration: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Reserve(Reservation{Key: "org/r:fail", Repository: "org/r", HourlyRateUSD: 0.90}); err != nil {
		t.Fatal(err)
	}
	tracker.Cancel("org/r:fail")
	snap := tracker.Snapshot()
	if snap.DailySpendUSD != 0 || snap.MonthlySpendUSD != 0 || snap.InFlightSpendUSD != 0 {
		t.Fatalf("spend after Cancel = %#v, want 0", snap)
	}
}

func TestReservationRetriesFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	tracker, err := NewTracker(Config{DailyCapUSD: 1, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := tracker.Reserve(Reservation{Key: "job", HourlyRateUSD: 0.1}); err == nil {
			t.Fatal("accepted reservation with unwritable ledger")
		}
	}
	if tracker.Snapshot().InFlightSpendUSD != 0 {
		t.Fatal("failed reservation counted as in flight")
	}
}
