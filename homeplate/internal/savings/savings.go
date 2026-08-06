// Package savings computes the "$ saved this month" counter.
//
// ACCURACY MATTERS HERE. This number is the headline claim, so the
// implementation is deliberately conservative and every assumption is
// inspectable via `homeplate status --explain-savings`:
//
//   - Only jobs that actually RAN to completion on this machine count.
//   - Billing is rounded UP to the whole minute, per job, exactly as GitHub
//     does ("each job is rounded up to the nearest minute").
//   - The per-minute rate is the class rate GitHub charges, which ALREADY
//     embeds the multiplier (Linux 1x, Windows 2x, macOS 10x).
//   - By default the counter does NOT claim savings on minutes that would have
//     been covered by an account's included free tier, because those minutes
//     were free. Set count_free_minutes = true to include them.
//   - Since March 2026 GitHub charges $0.002/min for self-hosted job runtime
//     on PRIVATE repos (a control-plane fee; public repos are free). The
//     headline figure is therefore NET: gross hosted-equivalent savings minus
//     that fee for every private-repo minute.
package savings

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/labels"
	"github.com/homeplate-ci/homeplate/internal/store"
)

// RateTable is GitHub's published PAID per-minute price for standard hosted
// runners, verified against GitHub's docs on RatesAsOf.
//
//	Linux 1-core   (x64)            actions_linux_slim   $0.002
//	Linux 2-core   (x64)            actions_linux        $0.006   <- default
//	Linux 2-core   (arm64)          actions_linux_arm    $0.005
//	Windows 2-core (x64/arm64)      actions_windows      $0.010
//	macOS 3/4-core (M1 or Intel)    actions_macos        $0.062
//
// IMPORTANT CORRECTION vs. common belief: the widely quoted "Linux 1x,
// Windows 2x, macOS 10x" multipliers apply ONLY to an account's INCLUDED FREE
// minutes. They do NOT apply to these paid per-minute rates, and the paid
// rates are not in a 1:2:10 ratio (Windows is ~1.67x Linux, macOS ~10.3x).
// Pricing the counter off multipliers would overstate savings by ~33% on
// Windows. Homeplate prices off the published paid rates only.
var RateTable = map[labels.Class]float64{
	labels.ClassLinux:   0.006,
	labels.ClassWindows: 0.010,
	labels.ClassMacOS:   0.062,
}

// RatesAsOf is the date RateTable was last verified against GitHub's docs.
// A stale table is visible in `--explain-savings` rather than silently wrong.
const RatesAsOf = "2026-08-06"

// SelfHostedFeePerMin is GitHub's control-plane fee per minute of self-hosted
// job runtime on PRIVATE repos, introduced March 2026. Public repos are
// exempt. Running on your own machine is not fully free on private repos, so
// the savings counter subtracts this fee from the gross hosted-equivalent
// figure to report an honest NET saving.
const SelfHostedFeePerMin = 0.002

// IsPublicRepo reports whether a repo slug ("owner/repo") is a public repo.
// The CLI wires this up from the linked-repo config before computing savings.
// A nil hook treats every repo as private - i.e. the fee always applies -
// which is the conservative default.
var IsPublicRepo func(slug string) bool

// ratesFile mirrors the user-editable <home>/rates.json. Unknown fields are
// tolerated so newer files still load.
type ratesFile struct {
	AsOf      string             `json:"as_of"`
	Rates     map[string]float64 `json:"rates_usd_per_min"`
	FeePerMin *float64           `json:"self_hosted_fee_usd_per_min"`
}

// RatesSource is printed alongside the counter so the claim is auditable.
const RatesSource = "https://docs.github.com/en/billing/managing-billing-for-your-products/about-billing-for-github-actions"

// FreeMinuteMultipliers are GitHub's INCLUDED-MINUTE multipliers. They are
// shown in --explain-savings for context but are deliberately NOT used to
// compute dollars, because they do not apply to paid per-minute rates.
var FreeMinuteMultipliers = map[labels.Class]float64{
	labels.ClassLinux:   1,
	labels.ClassWindows: 2,
	labels.ClassMacOS:   10,
}

// Calculator prices local job time.
type Calculator struct {
	rates map[labels.Class]float64
	// feePerMin is the self-hosted control-plane fee charged on private repos.
	feePerMin float64
	// Overridden records which classes came from user config.
	overridden map[labels.Class]bool
	// ratesNote records where the rates came from (or why the file was
	// ignored) so the audit output can disclose it.
	ratesNote string
}

// New builds a calculator. Rates resolve in ascending priority: built-in
// RateTable, then <home>/rates.json (user-editable; created with the current
// defaults on first run), then config RateOverrideUSDPerMin, which always
// wins. A malformed rates.json falls back to the built-in rates and records
// the fact in ratesNote rather than silently mispricing.
func New(cfg config.SavingsCfg) *Calculator {
	c := &Calculator{
		rates:      map[labels.Class]float64{},
		feePerMin:  SelfHostedFeePerMin,
		overridden: map[labels.Class]bool{},
	}
	for k, v := range RateTable {
		c.rates[k] = v
	}
	c.loadRatesFile()
	for name, rate := range cfg.RateOverrideUSDPerMin {
		if rate <= 0 {
			continue
		}
		class := labels.Class(strings.ToLower(strings.TrimSpace(name)))
		if _, ok := c.rates[class]; ok {
			c.rates[class] = rate
			c.overridden[class] = true
		}
	}
	return c
}

// loadRatesFile applies <home>/rates.json if it exists, seeding it with the
// current defaults when it does not so users can edit it.
func (c *Calculator) loadRatesFile() {
	path := filepath.Join(config.Dir(), "rates.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fee := SelfHostedFeePerMin
		def := ratesFile{
			AsOf: RatesAsOf,
			Rates: map[string]float64{
				string(labels.ClassLinux):   RateTable[labels.ClassLinux],
				string(labels.ClassWindows): RateTable[labels.ClassWindows],
				string(labels.ClassMacOS):   RateTable[labels.ClassMacOS],
			},
			FeePerMin: &fee,
		}
		b, merr := json.MarshalIndent(def, "", "  ")
		if merr != nil {
			return
		}
		// Best-effort: a read-only home dir must not break the counter.
		if os.MkdirAll(config.Dir(), 0o700) == nil {
			_ = os.WriteFile(path, b, 0o600)
		}
		return
	}
	if err != nil {
		c.ratesNote = fmt.Sprintf("could not read %s (%v); using built-in rates verified %s", path, err, RatesAsOf)
		return
	}
	var rf ratesFile
	if err := json.Unmarshal(data, &rf); err != nil {
		c.ratesNote = fmt.Sprintf("%s is malformed (%v); using built-in rates verified %s", path, err, RatesAsOf)
		return
	}
	for name, rate := range rf.Rates {
		if rate <= 0 {
			continue
		}
		class := labels.Class(strings.ToLower(strings.TrimSpace(name)))
		if _, ok := c.rates[class]; ok {
			c.rates[class] = rate
		}
	}
	if rf.FeePerMin != nil && *rf.FeePerMin >= 0 {
		c.feePerMin = *rf.FeePerMin
	}
	asOf := rf.AsOf
	if asOf == "" {
		asOf = "unknown date"
	}
	c.ratesNote = fmt.Sprintf("rates loaded from %s (as_of %s)", path, asOf)
}

// Rate returns the per-minute USD rate for a class.
func (c *Calculator) Rate(class labels.Class) float64 {
	if r, ok := c.rates[class]; ok {
		return r
	}
	return c.rates[labels.ClassLinux]
}

// BillableMinutes applies GitHub's rounding rule: every job is rounded up to
// the nearest whole minute, and a job that ran at all bills at least 1 minute.
func BillableMinutes(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	m := int(math.Ceil(d.Minutes()))
	if m < 1 {
		m = 1
	}
	return m
}

// ClassBreakdown is per-runner-class detail.
type ClassBreakdown struct {
	Class      labels.Class
	Jobs       int
	Minutes    int
	Rate       float64
	USD        float64
	Overridden bool
}

// Summary is the full savings picture for a period.
type Summary struct {
	From    time.Time
	To      time.Time
	Jobs    int
	Minutes int
	// USD is the NET saving: GrossUSD minus FeeUSD. This is the headline.
	USD float64
	// GrossUSD is what the same minutes would have cost on hosted runners.
	GrossUSD float64
	// FeeUSD is GitHub's self-hosted control-plane fee on private-repo
	// minutes (SelfHostedFeePerMin since March 2026; public repos exempt).
	FeeUSD float64
	// FeeRate is the per-minute fee rate actually applied.
	FeeRate float64
	ByClass []ClassBreakdown
	// RatesNote discloses where the rates came from, if not the built-ins.
	RatesNote string
	// Skipped counts jobs excluded from the total and why.
	SkippedJobs int
	SkipReasons map[string]int
}

// Compute prices a set of jobs.
//
// Jobs are counted only when they reached a terminal state having actually
// executed. Queued, cancelled, and interrupted jobs are excluded: GitHub would
// not have billed a full run for work that did not happen, and inflating the
// counter with them would make the headline number dishonest.
func (c *Calculator) Compute(jobs []*store.Job, from, to time.Time) Summary {
	s := Summary{From: from, To: to, FeeRate: c.feePerMin, RatesNote: c.ratesNote, SkipReasons: map[string]int{}}
	byClass := map[labels.Class]*ClassBreakdown{}

	for _, j := range jobs {
		if j.FinishedAt == nil {
			s.SkippedJobs++
			s.SkipReasons["never finished"]++
			continue
		}
		if j.FinishedAt.Before(from) || j.FinishedAt.After(to) {
			continue
		}
		switch j.State {
		case store.StateSucceeded, store.StateFailed:
			// Both bill on GitHub: a failed job consumes minutes too.
		case store.StateCancelled:
			s.SkippedJobs++
			s.SkipReasons["cancelled"]++
			continue
		case store.StateInterrupted:
			s.SkippedJobs++
			s.SkipReasons["interrupted (machine slept)"]++
			continue
		default:
			s.SkippedJobs++
			s.SkipReasons["not finished"]++
			continue
		}

		dur := time.Duration(j.BillableSeconds * float64(time.Second))
		mins := BillableMinutes(dur)
		if mins == 0 {
			s.SkippedJobs++
			s.SkipReasons["zero duration"]++
			continue
		}

		class := labels.Class(j.RunnerClass)
		if _, ok := c.rates[class]; !ok {
			class = labels.ClassOf(j.RunsOn)
		}

		b, ok := byClass[class]
		if !ok {
			b = &ClassBreakdown{Class: class, Rate: c.Rate(class), Overridden: c.overridden[class]}
			byClass[class] = b
		}
		b.Jobs++
		b.Minutes += mins
		b.USD += float64(mins) * c.Rate(class)

		// GitHub's March 2026 control-plane fee applies to self-hosted
		// minutes on private repos only. A nil IsPublicRepo hook treats
		// every repo as private - the conservative default.
		if IsPublicRepo == nil || !IsPublicRepo(j.RepoSlug) {
			s.FeeUSD += float64(mins) * c.feePerMin
		}

		s.Jobs++
		s.Minutes += mins
	}

	// Subtotals are deliberately NOT rounded before summing. Rounding each
	// class to cents first and then adding them introduces accumulation error
	// (e.g. a single 1-minute Linux job is $0.008, which would round to $0.01
	// and overstate the total by 25%). Full precision is kept internally and
	// rounding happens only at display time.
	for _, b := range byClass {
		s.ByClass = append(s.ByClass, *b)
		s.GrossUSD += b.USD
	}
	// The headline is NET: gross hosted-equivalent savings minus GitHub's
	// self-hosted fee on private-repo minutes.
	s.USD = s.GrossUSD - s.FeeUSD
	sort.Slice(s.ByClass, func(i, j int) bool { return s.ByClass[i].USD > s.ByClass[j].USD })
	return s
}

// MonthToDate is the window the headline counter uses.
func MonthToDate(now time.Time) (from, to time.Time) {
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return from, now
}

// Format renders the headline line for `homeplate status`.
func (s Summary) Format() string {
	return fmt.Sprintf("$%.2f saved this month (net of $%.2f GitHub self-hosted fee; gross $%.2f)  (%d jobs, %d billable minutes)",
		s.USD, s.FeeUSD, s.GrossUSD, s.Jobs, s.Minutes)
}

// Explain renders the full audit trail for `--explain-savings`.
func (s Summary) Explain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Savings window: %s -> %s\n", s.From.Format("2006-01-02"), s.To.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Rates verified %s from %s\n", RatesAsOf, RatesSource)
	fmt.Fprintln(&b, "Rule: every job rounds UP to a whole minute, exactly as GitHub bills.")
	fmt.Fprintln(&b, "Rates are GitHub's PAID per-minute prices. The well-known 1x/2x/10x")
	fmt.Fprintln(&b, "multipliers apply only to included free minutes, not to paid rates, so")
	fmt.Fprintln(&b, "they are deliberately not used here.")
	fmt.Fprintf(&b, "Since March 2026 GitHub charges $%.3f/min for self-hosted job runtime on\n", s.FeeRate)
	fmt.Fprintln(&b, "PRIVATE repos (a control-plane fee; public repos are exempt). The headline")
	fmt.Fprintln(&b, "figure is NET: gross hosted-equivalent savings minus that fee.")
	if s.RatesNote != "" {
		fmt.Fprintf(&b, "Rates: %s\n", s.RatesNote)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "  %-10s %6s %9s %12s %12s\n", "CLASS", "JOBS", "MINUTES", "USD/MIN", "SUBTOTAL")
	for _, c := range s.ByClass {
		note := ""
		if c.Overridden {
			note = "  (rate overridden in config.toml)"
		}
		fmt.Fprintf(&b, "  %-10s %6d %9d %12.4f %12.2f%s\n",
			c.Class, c.Jobs, c.Minutes, c.Rate, c.USD, note)
	}
	fmt.Fprintf(&b, "  %-10s %6d %9d %12s %12.2f\n", "GROSS", s.Jobs, s.Minutes, "", s.GrossUSD)
	fmt.Fprintf(&b, "\nGitHub self-hosted fee (private repos, $%.4f/min): -$%.2f\n", s.FeeRate, s.FeeUSD)
	fmt.Fprintf(&b, "TOTAL NET SAVINGS: $%.2f\n", s.USD)

	if s.SkippedJobs > 0 {
		fmt.Fprintf(&b, "\nExcluded %d job(s) that GitHub would not have fully billed:\n", s.SkippedJobs)
		reasons := make([]string, 0, len(s.SkipReasons))
		for r := range s.SkipReasons {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		for _, r := range reasons {
			fmt.Fprintf(&b, "  %-30s %d\n", r, s.SkipReasons[r])
		}
	}
	fmt.Fprintln(&b, "\nHonest caveat: if your account still has unused INCLUDED minutes, some of")
	fmt.Fprintln(&b, "this would have cost $0 on GitHub anyway. Homeplate cannot see your remaining")
	fmt.Fprintln(&b, "free balance, so it prices every local minute at the paid rate. Private-repo")
	fmt.Fprintln(&b, "free tiers are 2,000 min/mo (Pro) or 3,000 (Team), and public repos are free")
	fmt.Fprintln(&b, "on hosted runners - so for a purely public-repo workload the true saving is")
	fmt.Fprintln(&b, "$0. The self-hosted fee subtraction keys off the visibility recorded in your")
	fmt.Fprintln(&b, "linked-repo config; any repo not marked public is treated as private (fee")
	fmt.Fprintln(&b, "applies), which is the conservative assumption.")
	fmt.Fprintln(&b, "See README \"Is the savings counter honest?\".")
	return b.String()
}
