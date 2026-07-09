package main

import (
	"os"
	"testing"
	"time"
)

func TestParseCronRejectsBad(t *testing.T) {
	bad := []string{
		"",
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // day-of-month too low
		"* * * 13 *",  // month out of range
		"*/0 * * * *", // zero step
		"5-1 * * * *", // inverted range
		"abc * * * *", // non-numeric
	}
	for _, spec := range bad {
		if _, err := parseCron(spec); err == nil {
			t.Errorf("expected %q to be rejected", spec)
		}
	}
}

func TestCronMatches(t *testing.T) {
	// 2026-07-09 is a Thursday (weekday 4).
	thu := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		spec string
		t    time.Time
		want bool
	}{
		{"0 9 * * *", thu, true}, // 09:00 daily
		{"0 9 * * *", time.Date(2026, 7, 9, 9, 1, 0, 0, time.UTC), false}, // 09:01 no
		{"* * * * *", thu, true}, // every minute
		{"*/30 * * * *", time.Date(2026, 7, 9, 9, 30, 0, 0, time.UTC), true},
		{"*/30 * * * *", time.Date(2026, 7, 9, 9, 15, 0, 0, time.UTC), false},
		{"0 8 * * 1", time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC), true}, // Monday 08:00
		{"0 8 * * 1", thu, false},  // Thursday, no
		{"0 9 9 7 *", thu, true},   // 9 July 09:00
		{"0 9 10 7 *", thu, false}, // wrong day-of-month
		// dom AND dow both restricted → OR semantics: matches on either.
		{"0 9 9 * 1", thu, true},  // dom=9 matches even though dow=Mon doesn't
		{"0 9 1 * 4", thu, true},  // dow=Thu matches even though dom=1 doesn't
		{"0 9 1 * 1", thu, false}, // neither matches
		{"0 9 * * 0", time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC), true}, // Sunday via 0
		{"0 9 * * 7", time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC), true}, // Sunday via 7
	}
	for _, c := range cases {
		s, err := parseCron(c.spec)
		if err != nil {
			t.Fatalf("parse %q: %v", c.spec, err)
		}
		if got := s.matches(c.t); got != c.want {
			t.Errorf("%q @ %s: got %v want %v", c.spec, c.t.Format("Mon 15:04"), got, c.want)
		}
	}
}

func TestCronJobLifecycle(t *testing.T) {
	home = t.TempDir()
	if err := os.MkdirAll(statePath(""), 0o700); err != nil {
		t.Fatal(err)
	}
	cronJobs = nil
	activeAgent = defaultAgent
	currentTopic = 0

	job, err := addCronJob("*/5 * * * *", "check the news")
	if err != nil {
		t.Fatalf("addCronJob: %v", err)
	}
	if job.ID == "" || job.Agent != defaultAgent || !job.Enabled {
		t.Errorf("unexpected job: %+v", job)
	}

	// A bad schedule must not create a job.
	if _, err := addCronJob("not a cron", "x"); err == nil {
		t.Error("expected bad schedule to be rejected")
	}

	// Persisted across a reload.
	loadCronJobs()
	if len(cronJobs) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(cronJobs))
	}

	due := dueCronJobs(time.Date(2026, 7, 9, 9, 5, 0, 0, time.UTC))
	if len(due) != 1 {
		t.Errorf("expected job due at 09:05, got %d", len(due))
	}
	if d := dueCronJobs(time.Date(2026, 7, 9, 9, 6, 0, 0, time.UTC)); len(d) != 0 {
		t.Errorf("expected no job due at 09:06, got %d", len(d))
	}

	found, err := removeCronJob(job.ID)
	if err != nil || !found {
		t.Errorf("remove failed: found=%v err=%v", found, err)
	}
	if ok, _ := removeCronJob(job.ID); ok {
		t.Error("second remove should report not found")
	}
}
