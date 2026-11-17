package auth

import (
	"strconv"
	"testing"
	"time"
)

func TestEnterprisePaidDailyRecoveryAtAcrossPacificDST(t *testing.T) {
	la, errLocation := time.LoadLocation("America/Los_Angeles")
	if errLocation != nil {
		t.Fatal(errLocation)
	}
	for _, test := range []struct {
		name      string
		month     time.Month
		day       int
		resetHour int
	}{
		{name: "spring standard midnight", month: time.March, day: 8, resetHour: 8},
		{name: "spring daylight midnight", month: time.March, day: 9, resetHour: 7},
		{name: "autumn daylight midnight", month: time.November, day: 1, resetHour: 7},
		{name: "autumn standard midnight", month: time.November, day: 2, resetHour: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetAt := time.Date(2026, test.month, test.day, 0, 0, 0, 0, la)
			if resetAt.UTC().Hour() != test.resetHour {
				t.Fatalf("unexpected Pacific midnight fixture: %s", resetAt)
			}
			const poolID = "fixture-paid-daily-pool"
			got := enterprisePaidDailyRecoveryAt(resetAt, poolID)
			start := time.Date(2026, test.month, test.day, 8, 2, 0, 0, time.UTC)
			end := start.Add(3 * time.Minute)
			if got.Before(start) || got.After(end) || got.Before(resetAt) {
				t.Fatalf("recovery at %s is outside [%s, %s] after reset %s", got, start, end, resetAt)
			}
		})
	}
}

func TestEnterprisePaidDailyRecoveryAtHonorsExplicitReset(t *testing.T) {
	start := time.Date(2026, time.November, 17, 8, 2, 0, 0, time.UTC)
	end := start.Add(3 * time.Minute)
	const poolID = "fixture-paid-daily-pool"
	readyAt := enterprisePaidDailyRecoveryAt(start.Add(-time.Minute), poolID)
	for _, test := range []struct {
		name  string
		reset time.Time
		want  time.Time
	}{
		{name: "missing reset", reset: time.Time{}, want: time.Time{}},
		{name: "before window", reset: start.Add(-time.Second), want: readyAt},
		{name: "at assigned recovery", reset: readyAt, want: readyAt},
		{name: "window end", reset: end, want: end},
		{name: "after window", reset: end.Add(30 * time.Second), want: end.Add(30 * time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := enterprisePaidDailyRecoveryAt(test.reset, poolID); !got.Equal(test.want) {
				t.Fatalf("recovery for reset %s = %s, want %s", test.reset, got, test.want)
			}
		})
	}
}

func TestEnterprisePaidDailyRecoveryAtStaggersPoolsDeterministically(t *testing.T) {
	resetAt := time.Date(2026, time.November, 17, 8, 0, 0, 0, time.UTC)
	offsets := make(map[time.Duration]bool)
	for index := range 12 {
		poolID := "fixture-paid-pool-" + strconv.Itoa(index)
		readyAt := enterprisePaidDailyRecoveryAt(resetAt, poolID)
		if got := enterprisePaidDailyRecoveryAt(resetAt.In(enterpriseRecoveryLocation), poolID); !got.Equal(readyAt) {
			t.Fatalf("the same reset instant produced different recovery times: %s, %s", readyAt, got)
		}
		next := enterprisePaidDailyRecoveryAt(resetAt.AddDate(0, 0, 1), poolID)
		if !next.Equal(readyAt.AddDate(0, 0, 1)) {
			t.Fatalf("the pool offset changed across quota days: %s, %s", readyAt, next)
		}
		offsets[readyAt.Sub(resetAt)] = true
	}
	if len(offsets) < 2 {
		t.Fatal("all quota pools were scheduled at the same time")
	}
}
