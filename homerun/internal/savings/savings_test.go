package savings

import (
	"strings"
	"testing"
	"time"

	"github.com/homerun-ci/homerun/internal/config"
	"github.com/homerun-ci/homerun/internal/labels"
	"github.com/homerun-ci/homerun/internal/store"
)

func job(class string, seconds float64, state store.JobState, finished time.Time) *store.Job {
	return &store.Job{
		RunnerClass: class, BillableSeconds: seconds, State: state, FinishedAt: &finished,
	}
}

// TestGitHubRoundingRule: GitHub rounds every job UP to the whole minute.
// Getting this wrong understates or overstates the headline number.
func TestGitHubRoundingRule(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int
	}{
		{0, 0},
		{1 * time.Second, 1},
		{59 * time.Second, 1},
		{60 * time.Second, 1},
		{61 * time.Second, 2},
		{5 * time.Minute, 5},
		{5*time.Minute + 1*time.Second, 6},
	}
	for _, c := range cases {
		if got := BillableMinutes(c.d); got != c.want {
			t.Errorf("BillableMinutes(%s) = %d, want %d", c.d, got, c.want)
		}
	}
}

// TestRatesMatchGitHubPublishedPrices pins the exact paid per-minute rates
// verified from GitHub's billing docs. If GitHub changes pricing, this test
// fails loudly instead of the headline number quietly drifting.
func TestRatesMatchGitHubPublishedPrices(t *testing.T) {
	want := map[labels.Class]float64{
		labels.ClassLinux:   0.006, // Linux 2-core x64  (actions_linux)
		labels.ClassWindows: 0.010, // Windows 2-core    (actions_windows)
		labels.ClassMacOS:   0.062, // macOS 3/4-core    (actions_macos)
	}
	for class, w := range want {
		if got := RateTable[class]; got != w {
			t.Errorf("%s rate = %.4f, want %.4f (verified %s from %s)",
				class, got, w, RatesAsOf, RatesSource)
		}
	}
}

// TestMultipliersAreNotUsedForPricing guards the correction that cost real
// accuracy: the 1x/2x/10x multipliers apply to INCLUDED FREE minutes only.
// Applying them to paid rates would overstate Windows savings by ~33%.
func TestMultipliersAreNotUsedForPricing(t *testing.T) {
	base := RateTable[labels.ClassLinux]
	for class, mult := range FreeMinuteMultipliers {
		if class == labels.ClassLinux {
			continue
		}
		if RateTable[class] == base*mult {
			t.Errorf("%s paid rate equals Linux x%.0f, which means the free-minute "+
				"multiplier was wrongly applied to paid pricing", class, mult)
		}
	}
}

// TestPricingIsAccurate checks a worked example end to end.
func TestPricingIsAccurate(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{})

	jobs := []*store.Job{
		// 10 exact minutes of Linux -> 10 min x $0.006 = $0.06
		job("linux", 600, store.StateSucceeded, now),
		// 61 seconds of macOS -> rounds UP to 2 min x $0.062 = $0.124
		job("macos", 61, store.StateSucceeded, now),
	}
	sum := c.Compute(jobs, from, to)

	if sum.Minutes != 12 {
		t.Errorf("Minutes = %d, want 12 (10 + 2 after rounding)", sum.Minutes)
	}
	const want = 10*0.006 + 2*0.062
	if diff := sum.USD - want; diff > 0.005 || diff < -0.005 {
		t.Errorf("USD = %.4f, want %.4f", sum.USD, want)
	}
	if sum.Jobs != 2 {
		t.Errorf("Jobs = %d, want 2", sum.Jobs)
	}
}

// TestFailedJobsStillCount: GitHub bills failed jobs, so Homerun must credit
// them as savings too. Excluding them would understate the counter.
func TestFailedJobsStillCount(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{})
	sum := c.Compute([]*store.Job{job("linux", 120, store.StateFailed, now)}, from, to)
	if sum.Jobs != 1 || sum.Minutes != 2 {
		t.Errorf("failed job not counted: jobs=%d minutes=%d", sum.Jobs, sum.Minutes)
	}
}

// TestCancelledAndInterruptedAreExcluded: the counter must not inflate itself
// with work GitHub would not have fully billed.
func TestCancelledAndInterruptedAreExcluded(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{})

	sum := c.Compute([]*store.Job{
		job("linux", 600, store.StateCancelled, now),
		job("linux", 600, store.StateInterrupted, now),
		job("linux", 600, store.StateSucceeded, now),
	}, from, to)

	if sum.Jobs != 1 {
		t.Errorf("Jobs = %d, want 1 (cancelled and interrupted excluded)", sum.Jobs)
	}
	if sum.SkippedJobs != 2 {
		t.Errorf("SkippedJobs = %d, want 2", sum.SkippedJobs)
	}
	if len(sum.SkipReasons) == 0 {
		t.Error("skip reasons must be recorded so the number is auditable")
	}
}

// TestOutOfWindowJobsExcluded checks the month boundary.
func TestOutOfWindowJobsExcluded(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	lastMonth := from.AddDate(0, 0, -5)

	c := New(config.SavingsCfg{})
	sum := c.Compute([]*store.Job{
		job("linux", 600, store.StateSucceeded, lastMonth),
		job("linux", 600, store.StateSucceeded, now),
	}, from, to)

	if sum.Jobs != 1 {
		t.Errorf("Jobs = %d, want 1; last month's jobs must not count toward this month", sum.Jobs)
	}
}

// TestRateOverride lets users correct the table if GitHub changes pricing.
func TestRateOverride(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{RateOverrideUSDPerMin: map[string]float64{"linux": 0.02}})

	sum := c.Compute([]*store.Job{job("linux", 60, store.StateSucceeded, now)}, from, to)
	if diff := sum.USD - 0.02; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("USD = %.4f, want 0.02 from the override", sum.USD)
	}
	if len(sum.ByClass) != 1 || !sum.ByClass[0].Overridden {
		t.Error("overridden rates must be flagged so the audit output is honest")
	}
}

// TestExplainIsAuditable verifies the --explain-savings output discloses its
// sources and assumptions, since this is the marketing number.
func TestExplainIsAuditable(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{})
	sum := c.Compute([]*store.Job{
		job("linux", 600, store.StateSucceeded, now),
		job("macos", 600, store.StateCancelled, now),
	}, from, to)

	text := sum.Explain()
	for _, must := range []string{RatesSource, RatesAsOf, "rounds UP", "TOTAL", "Excluded"} {
		if !strings.Contains(text, must) {
			t.Errorf("Explain() is missing %q; the savings claim must be auditable:\n%s", must, text)
		}
	}
}

// TestZeroDurationExcluded stops phantom jobs inflating the counter.
func TestZeroDurationExcluded(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{})
	sum := c.Compute([]*store.Job{job("linux", 0, store.StateSucceeded, now)}, from, to)
	if sum.Jobs != 0 || sum.USD != 0 {
		t.Errorf("zero-duration job counted: jobs=%d usd=%.4f", sum.Jobs, sum.USD)
	}
}

// TestUnknownClassFallsBackToCheapest keeps the counter conservative.
func TestUnknownClassFallsBackToCheapest(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	c := New(config.SavingsCfg{})
	j := job("", 60, store.StateSucceeded, now)
	j.RunsOn = "some-custom-label"
	sum := c.Compute([]*store.Job{j}, from, to)
	if diff := sum.USD - RateTable[labels.ClassLinux]; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("unknown class priced at %.4f, want the cheapest (linux) rate %.4f",
			sum.USD, RateTable[labels.ClassLinux])
	}
}

// TestClassDetection maps runs-on strings to cost classes.
func TestClassDetection(t *testing.T) {
	cases := map[string]labels.Class{
		"ubuntu-latest": labels.ClassLinux,
		"macos-14":      labels.ClassMacOS,
		"macos-latest":  labels.ClassMacOS,
		"windows-2022":  labels.ClassWindows,
		"homerun-macos": labels.ClassMacOS,
		"self-hosted":   labels.ClassLinux,
	}
	for in, want := range cases {
		if got := labels.ClassOf(in); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", in, got, want)
		}
	}
}
