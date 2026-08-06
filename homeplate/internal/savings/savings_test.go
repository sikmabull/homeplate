package savings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/labels"
	"github.com/homeplate-ci/homeplate/internal/store"
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
	const wantGross = 10*0.006 + 2*0.062
	if diff := sum.GrossUSD - wantGross; diff > 0.005 || diff < -0.005 {
		t.Errorf("GrossUSD = %.4f, want %.4f", sum.GrossUSD, wantGross)
	}
	// IsPublicRepo is nil here, so every repo is treated as private and the
	// March 2026 self-hosted fee applies: 12 min x $0.002 = $0.024.
	const wantFee = 12 * SelfHostedFeePerMin
	if diff := sum.FeeUSD - wantFee; diff > 0.005 || diff < -0.005 {
		t.Errorf("FeeUSD = %.4f, want %.4f", sum.FeeUSD, wantFee)
	}
	if diff := sum.USD - (wantGross - wantFee); diff > 0.005 || diff < -0.005 {
		t.Errorf("USD = %.4f, want net %.4f (gross - fee)", sum.USD, wantGross-wantFee)
	}
	if sum.Jobs != 2 {
		t.Errorf("Jobs = %d, want 2", sum.Jobs)
	}
}

// TestFailedJobsStillCount: GitHub bills failed jobs, so Homeplate must credit
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
	if diff := sum.GrossUSD - 0.02; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("GrossUSD = %.4f, want 0.02 from the override", sum.GrossUSD)
	}
	// 1 billable minute, nil visibility hook -> private -> fee applies.
	if diff := sum.USD - (0.02 - SelfHostedFeePerMin); diff > 0.0001 || diff < -0.0001 {
		t.Errorf("USD = %.4f, want net %.4f", sum.USD, 0.02-SelfHostedFeePerMin)
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
	if diff := sum.GrossUSD - RateTable[labels.ClassLinux]; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("unknown class priced at %.4f, want the cheapest (linux) rate %.4f",
			sum.GrossUSD, RateTable[labels.ClassLinux])
	}
}

// TestClassDetection maps runs-on strings to cost classes.
func TestClassDetection(t *testing.T) {
	cases := map[string]labels.Class{
		"ubuntu-latest":   labels.ClassLinux,
		"macos-14":        labels.ClassMacOS,
		"macos-latest":    labels.ClassMacOS,
		"windows-2022":    labels.ClassWindows,
		"homeplate-macos": labels.ClassMacOS,
		"self-hosted":     labels.ClassLinux,
	}
	for in, want := range cases {
		if got := labels.ClassOf(in); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPrivateRepoFeeSubtracted: since March 2026 GitHub charges $0.002/min for
// self-hosted runtime on private repos, so the headline must be NET.
func TestPrivateRepoFeeSubtracted(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	old := IsPublicRepo
	IsPublicRepo = func(slug string) bool { return slug == "acme/public" }
	t.Cleanup(func() { IsPublicRepo = old })

	c := New(config.SavingsCfg{})
	j := job("linux", 600, store.StateSucceeded, now) // 10 billable minutes
	j.RepoSlug = "acme/private"
	sum := c.Compute([]*store.Job{j}, from, to)

	const wantGross = 10 * 0.006
	const wantFee = 10 * SelfHostedFeePerMin
	if diff := sum.GrossUSD - wantGross; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("GrossUSD = %.4f, want %.4f", sum.GrossUSD, wantGross)
	}
	if diff := sum.FeeUSD - wantFee; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("FeeUSD = %.4f, want %.4f (10 min x $%.3f)", sum.FeeUSD, wantFee, SelfHostedFeePerMin)
	}
	if diff := sum.USD - (wantGross - wantFee); diff > 0.0001 || diff < -0.0001 {
		t.Errorf("USD = %.4f, want net %.4f", sum.USD, wantGross-wantFee)
	}
}

// TestPublicRepoNoFee: public repos are exempt from the self-hosted
// control-plane fee, so net equals gross.
func TestPublicRepoNoFee(t *testing.T) {
	now := time.Now()
	from, to := MonthToDate(now)
	old := IsPublicRepo
	IsPublicRepo = func(slug string) bool { return slug == "acme/public" }
	t.Cleanup(func() { IsPublicRepo = old })

	c := New(config.SavingsCfg{})
	j := job("linux", 600, store.StateSucceeded, now)
	j.RepoSlug = "acme/public"
	sum := c.Compute([]*store.Job{j}, from, to)

	if sum.FeeUSD != 0 {
		t.Errorf("FeeUSD = %.4f, want 0 for a public repo", sum.FeeUSD)
	}
	const wantGross = 10 * 0.006
	if diff := sum.USD - wantGross; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("USD = %.4f, want %.4f (net == gross when no fee)", sum.USD, wantGross)
	}
}

// TestRatesFileOverridesBuiltins: a user-edited rates.json must win over the
// built-in table, tolerate unknown fields, and still lose to config.toml
// rate_override_usd_per_min.
func TestRatesFileOverridesBuiltins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOMEPLATE_HOME", home)
	ratesJSON := `{
		"as_of": "2026-09-01",
		"rates_usd_per_min": {"linux": 0.02},
		"self_hosted_fee_usd_per_min": 0.005,
		"some_future_field": true
	}`
	if err := os.WriteFile(filepath.Join(home, "rates.json"), []byte(ratesJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	from, to := MonthToDate(now)

	c := New(config.SavingsCfg{})
	if got := c.Rate(labels.ClassLinux); got != 0.02 {
		t.Errorf("linux rate = %.4f, want 0.02 from rates.json", got)
	}
	if got := c.Rate(labels.ClassMacOS); got != RateTable[labels.ClassMacOS] {
		t.Errorf("macos rate = %.4f, want built-in %.4f (not in rates.json)", got, RateTable[labels.ClassMacOS])
	}
	j := job("linux", 600, store.StateSucceeded, now) // 10 min
	sum := c.Compute([]*store.Job{j}, from, to)
	if diff := sum.GrossUSD - 10*0.02; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("GrossUSD = %.4f, want %.4f from rates.json rate", sum.GrossUSD, 10*0.02)
	}
	if diff := sum.FeeUSD - 10*0.005; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("FeeUSD = %.4f, want %.4f from rates.json fee", sum.FeeUSD, 10*0.005)
	}

	// config.toml override still wins over rates.json.
	c2 := New(config.SavingsCfg{RateOverrideUSDPerMin: map[string]float64{"linux": 0.03}})
	if got := c2.Rate(labels.ClassLinux); got != 0.03 {
		t.Errorf("linux rate = %.4f, want 0.03: config override must beat rates.json", got)
	}
}

// TestRatesFileCreatedWhenMissing: first run seeds <home>/rates.json with the
// current defaults so users can edit it.
func TestRatesFileCreatedWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOMEPLATE_HOME", home)

	c := New(config.SavingsCfg{})
	data, err := os.ReadFile(filepath.Join(home, "rates.json"))
	if err != nil {
		t.Fatalf("rates.json was not seeded: %v", err)
	}
	for _, must := range []string{`"as_of"`, `"rates_usd_per_min"`, `"self_hosted_fee_usd_per_min"`, "0.002", "0.006", "0.062"} {
		if !strings.Contains(string(data), must) {
			t.Errorf("seeded rates.json missing %s:\n%s", must, data)
		}
	}
	if got := c.Rate(labels.ClassLinux); got != RateTable[labels.ClassLinux] {
		t.Errorf("linux rate = %.4f, want built-in %.4f", got, RateTable[labels.ClassLinux])
	}
}

// TestMalformedRatesFileFallsBack: an unparseable rates.json must not break
// the counter; built-in rates apply and the fallback is disclosed.
func TestMalformedRatesFileFallsBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOMEPLATE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "rates.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New(config.SavingsCfg{})
	for class, want := range RateTable {
		if got := c.Rate(class); got != want {
			t.Errorf("%s rate = %.4f, want built-in %.4f after malformed rates.json", class, got, want)
		}
	}
	if c.feePerMin != SelfHostedFeePerMin {
		t.Errorf("fee = %.4f, want built-in %.4f after malformed rates.json", c.feePerMin, SelfHostedFeePerMin)
	}
	if c.ratesNote == "" || !strings.Contains(c.ratesNote, "malformed") {
		t.Errorf("ratesNote should disclose the malformed file, got %q", c.ratesNote)
	}
	now := time.Now()
	from, to := MonthToDate(now)
	sum := c.Compute([]*store.Job{job("linux", 600, store.StateSucceeded, now)}, from, to)
	if sum.RatesNote == "" {
		t.Error("Summary.RatesNote should carry the fallback disclosure into --explain-savings")
	}
}
