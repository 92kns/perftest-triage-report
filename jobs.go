package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===================== Per-job success rate =====================
//
// This mirrors the Redash dashboard query that runs directly against
// Treeherder's Postgres `job` table:
//
//	COUNT(CASE WHEN result = 'success' THEN 1 END) * 100.0 / COUNT(*)
//	  ... GROUP BY job_type.name, job.tier
//
// We reproduce it over the public REST API. Two constraints shaped the design:
//
//  1. /api/project/{repo}/jobs/ has no aggregate/count endpoint, so totals are
//     obtained by paginating rows and tallying client-side.
//  2. The API silently ignores unknown filters. `job_type_name__contains` is
//     accepted but NOT applied (it returns unrelated mochitest/wpt rows), so
//     the SQL's LIKE '%browsertime%' cannot be pushed server-side. Exact
//     `job_type_name`, `result`, `tier`, `job_group_symbol`, `end_time__gt`
//     and `end_time__lt` were each verified to genuinely filter.
//
// Filtering by job_group_symbol is what makes this affordable: it narrows
// ~1M jobs/week down to ~63k perf jobs, fetched in ~110 requests.

// perfRepos matches the dashboard's `repository_id IN (1, 6, 77)`.
var perfRepos = []string{"autoland", "mozilla-central", "mozilla-beta"}

// perfJobGroups enumerates the Treeherder job_group_symbol values whose jobs
// match the dashboard's name filters (browsertime / talos / awsy / perftest).
//
// This has to be an explicit list because the jobs API supports neither prefix
// matching on the symbol nor a job-group listing endpoint (/api/jobgroup/ and
// friends return the SPA's HTML). The set is open-ended in practice: the
// network-benchmark harness mints a new group per bandwidth/latency condition
// (Btime-300M_80ms, Btime-CaR-1M_400ms, ...), so new symbols appear over time.
//
// Run with --discover-job-groups to scan recent jobs and print any perf group
// not listed here.
var perfJobGroups = []string{
	"Btime", "Btime-100M_40ms", "Btime-10M_40ms", "Btime-1M_400ms",
	"Btime-300M_40ms", "Btime-300M_80ms", "Btime-CaR", "Btime-CaR-100M_40ms",
	"Btime-CaR-10M_40ms", "Btime-CaR-1M_400ms", "Btime-CaR-300M_40ms",
	"Btime-CaR-300M_80ms", "Btime-ChR", "Btime-Prof", "Btime-Saf",
	"Btime-cache", "Btime-fenix", "Btime-no-nv", "Btime-nofis-CaR",
	"Btime-nofis-ChR", "Btime-nofis-fenix", "Btime-nv", "Btime-webext",
	"Btime-webext-fenix", "Btime-webext-nofis-fenix",
	"SY",
	"T", "T-Prof", "T-no-nv", "T-nv", "T-swr",
	"perftest", "perftest-chrome", "perftest-fenix",
}

// perfJobGroupKeywords identifies a perf job by name, mirroring the dashboard's
// LIKE clauses. Used only by --discover-job-groups.
var perfJobGroupKeywords = []string{"browsertime", "talos", "awsy", "perftest"}

const (
	jobsPageSize = 2000 // API caps a page at 2000; count=5000 returns nothing.
	jobsMaxPages = 40   // 80k jobs per repo/group; the busiest (Btime) uses 13.
)

// THJob is the subset of /api/project/{repo}/jobs/ we tally.
type THJob struct {
	JobTypeName string `json:"job_type_name"`
	Result      string `json:"result"`
	Tier        int    `json:"tier"`
}

type thJobsResponse struct {
	Results []THJob `json:"results"`
}

// JobStat is a per-job-type success tally, the direct analogue of one row of
// the dashboard's GROUP BY.
type JobStat struct {
	JobTypeName string
	Tier        int
	Total       int
	Success     int
	Rate        string // success percentage, e.g. "97.4%"
}

// SuiteRate is a success tally rolled up to a test-suite key so it can be
// joined onto the suite report.
type SuiteRate struct {
	Total   int
	Success int
}

// Percent renders the success percentage, or "" when there is nothing to divide.
func (s SuiteRate) Percent() string {
	if s.Total == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f%%", float64(s.Success)/float64(s.Total)*100)
}

// sideBySideHash matches the two trailing revision hashes that side-by-side
// jobs carry, e.g.
//
//	test-linux2404-64-shippable browsertime-tp6-firefox-youtube c4c52051e373 16732e03f66f
//
// The suffix is " <12 hex> <12 hex>" = 1+12+1+12 = 26 characters, which is why
// the dashboard strips exactly 26.
//
// The dashboard's predicate is just `~ '[a-f0-9]{12}$'`. This requires both
// hashes, which is stricter: it produces identical results on real job names
// while refusing to mangle a suite that merely happens to end in 12 hex chars.
var sideBySideHash = regexp.MustCompile(` [a-f0-9]{12} [a-f0-9]{12}$`)

// normalizeJobTypeName collapses the per-run hashes that side-by-side jobs
// carry so their runs aggregate into a single row, matching the dashboard's
// LEFT(name, LENGTH(name) - 26) || '-side-by-side'.
func normalizeJobTypeName(name string) string {
	if sideBySideHash.MatchString(name) {
		return name[:len(name)-26] + "-side-by-side"
	}
	return name
}

// jobTypeSuiteRe pulls the suite portion out of a job type name, e.g.
// "test-linux2404-64-shippable/opt-browsertime-tp6-firefox-imdb" ->
// "browsertime-tp6-firefox-imdb". Names without the platform/variant prefix
// (mozperftest's "perftest-android-hw-a55-...") are already suite keys.
var jobTypeSuiteRe = regexp.MustCompile(`^test-.+?/(?:opt|debug|pgo)-(.+)$`)

func suiteKeyFromJobType(name string) string {
	if m := jobTypeSuiteRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// exclusiveEnd converts an inclusive end date into the exclusive upper bound
// the API wants. Using "<end>T23:59:59" would drop any job finishing in that
// final second, so the bound is the start of the following day.
func exclusiveEnd(end string) string {
	t, err := time.Parse("2006-01-02", end)
	if err != nil {
		// Not a bare date; fall back to end-of-day rather than dropping the bound.
		return end + "T23:59:59"
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02") + "T00:00:00"
}

// fetchJobsPage retrieves one page of jobs for a repo/group/window.
// ok is false when the page could not be fetched or decoded; callers must not
// treat the rows gathered so far as a complete set in that case.
func fetchJobsPage(repo, group, start, end string, page int) (jobs []THJob, more, ok bool) {
	params := url.Values{}
	params.Set("count", strconv.Itoa(jobsPageSize))
	params.Set("offset", strconv.Itoa(page*jobsPageSize))
	params.Set("job_group_symbol", group)
	params.Set("end_time__gt", start+"T00:00:00")
	params.Set("end_time__lt", exclusiveEnd(end))

	u := fmt.Sprintf("%s/project/%s/jobs/?%s", treeherderBase, repo, params.Encode())
	resp, err := get(u)
	if err != nil {
		log.Printf("fetch jobs %s/%s page %d: %v", repo, group, page, err)
		return nil, false, false
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("warning: error closing body: %v", err)
		}
	}()

	var out thJobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Printf("decode jobs %s/%s page %d: %v", repo, group, page, err)
		return nil, false, false
	}
	return out.Results, len(out.Results) == jobsPageSize, true
}

// fetchJobGroup pages through every job in one repo/group/window.
//
// complete is false if any page failed or the page cap was hit. A success rate
// built from a truncated sweep is not merely incomplete, it is WRONG -- the
// denominator shrinks while the surviving failures stay, inflating the failure
// rate. Callers must discard or flag incomplete sweeps rather than publish them.
func fetchJobGroup(repo, group, start, end string) (jobs []THJob, complete bool) {
	var all []THJob
	for page := 0; page < jobsMaxPages; page++ {
		got, more, ok := fetchJobsPage(repo, group, start, end, page)
		if !ok {
			log.Printf("warning: %s/%s sweep failed at page %d; dropping its counts", repo, group, page)
			return nil, false
		}
		all = append(all, got...)
		if !more {
			return all, true
		}
	}
	log.Printf("warning: %s/%s hit the %d-page cap; dropping its counts as truncated", repo, group, jobsMaxPages)
	return nil, false
}

// SweepStatus records how much of the job sweep actually succeeded, so the
// report can say so rather than presenting partial data as authoritative.
type SweepStatus struct {
	Total  int // repo x group sweeps attempted
	Failed int // sweeps dropped due to fetch failure or page-cap truncation
}

// Partial reports whether any sweep was dropped.
func (s SweepStatus) Partial() bool { return s.Failed > 0 }

// Warning renders a human-readable caveat, or "" when the sweep was clean.
func (s SweepStatus) Warning() string {
	if !s.Partial() {
		return ""
	}
	return fmt.Sprintf(
		"%d of %d job-group sweeps failed and were dropped. Success rates below are computed "+
			"from the remaining data and may understate the number of runs.", s.Failed, s.Total)
}

// fetchJobStats sweeps every perf job group across every perf repo and tallies
// success/total per job type. Returns rows sorted worst-success-rate first,
// plus how much of the sweep succeeded.
func fetchJobStats(start, end string) ([]JobStat, SweepStatus) {
	type key struct {
		name string
		tier int
	}
	tally := map[key]*JobStat{}
	status := SweepStatus{Total: len(perfRepos) * len(perfJobGroups)}

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, repo := range perfRepos {
		for _, group := range perfJobGroups {
			wg.Add(1)
			go func(repo, group string) {
				defer wg.Done()
				acquire()
				defer release()

				jobs, complete := fetchJobGroup(repo, group, start, end)
				mu.Lock()
				defer mu.Unlock()
				if !complete {
					status.Failed++
					return
				}
				for _, j := range jobs {
					k := key{normalizeJobTypeName(j.JobTypeName), j.Tier}
					st, ok := tally[k]
					if !ok {
						st = &JobStat{JobTypeName: k.name, Tier: k.tier}
						tally[k] = st
					}
					st.Total++
					if j.Result == "success" {
						st.Success++
					}
				}
			}(repo, group)
		}
	}
	wg.Wait()

	stats := make([]JobStat, 0, len(tally))
	for _, st := range tally {
		st.Rate = SuiteRate{Total: st.Total, Success: st.Success}.Percent()
		stats = append(stats, *st)
	}
	sortJobStats(stats)
	return stats, status
}

// sortJobStats orders by success rate ascending (worst first), then by total
// runs descending so a 0%-of-200 outranks a 0%-of-3.
func sortJobStats(stats []JobStat) {
	sort.Slice(stats, func(i, j int) bool {
		a, b := stats[i], stats[j]
		// Compare a.Success/a.Total < b.Success/b.Total without floats.
		// int64 so the cross-product cannot overflow on a 32-bit build, which
		// would make the comparator non-transitive and corrupt the sort.
		l := int64(a.Success) * int64(b.Total)
		r := int64(b.Success) * int64(a.Total)
		if l != r {
			return l < r
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.JobTypeName < b.JobTypeName
	})
}

// suiteRatesFrom rolls per-job-type tallies up to suite keys so they can be
// joined onto the suite report, which is keyed by Treeherder's `test_suite`.
func suiteRatesFrom(stats []JobStat) map[string]*SuiteRate {
	rates := map[string]*SuiteRate{}
	for _, st := range stats {
		k := suiteKeyFromJobType(st.JobTypeName)
		r, ok := rates[k]
		if !ok {
			r = &SuiteRate{}
			rates[k] = r
		}
		r.Total += st.Total
		r.Success += st.Success
	}
	return rates
}

// filterJobStats drops job types with too few runs to be meaningful and keeps
// only those below 100% success, matching the dashboard's
// `WHERE success_percentage < 100`.
//
// excludeAndroid mirrors the dashboard's `AND job_type.name NOT LIKE '%android%'`.
// It applies only to this displayed table -- android stats stay in the raw set
// so the Test Suites tab can still join rates onto android suites.
func filterJobStats(stats []JobStat, minRuns, limit int, excludeAndroid bool) []JobStat {
	out := make([]JobStat, 0, len(stats))
	for _, st := range stats {
		if st.Total < minRuns || st.Success == st.Total {
			continue
		}
		if excludeAndroid && strings.Contains(strings.ToLower(st.JobTypeName), "android") {
			continue
		}
		out = append(out, st)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// discoverJobGroups scans recent jobs and reports perf job groups that are not
// in perfJobGroups. Maintenance helper for --discover-job-groups.
//
// This is a SAMPLE, not an exhaustive scan: it reads the first
// jobsMaxPages*jobsPageSize jobs per repo, which is far fewer than a week's
// population. It reliably surfaces high-volume groups; a rare group may need
// several runs, or a narrower --days window, to show up. Finding nothing means
// "nothing in the sample", not "the list is provably complete".
func discoverJobGroups(start, end string) []string {
	type groupRow struct {
		JobTypeName    string `json:"job_type_name"`
		JobGroupSymbol string `json:"job_group_symbol"`
	}

	known := map[string]bool{}
	for _, g := range perfJobGroups {
		known[g] = true
	}

	found := map[string]string{} // symbol -> example job type
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, repo := range perfRepos {
		for page := 0; page < jobsMaxPages; page++ {
			wg.Add(1)
			go func(repo string, page int) {
				defer wg.Done()
				acquire()
				defer release()

				params := url.Values{}
				params.Set("count", strconv.Itoa(jobsPageSize))
				params.Set("offset", strconv.Itoa(page*jobsPageSize))
				params.Set("end_time__gt", start+"T00:00:00")
				params.Set("end_time__lt", exclusiveEnd(end))

				resp, err := get(fmt.Sprintf("%s/project/%s/jobs/?%s", treeherderBase, repo, params.Encode()))
				if err != nil {
					return
				}
				defer func() { _ = resp.Body.Close() }()

				var out struct {
					Results []groupRow `json:"results"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, r := range out.Results {
					if r.JobGroupSymbol == "" || known[r.JobGroupSymbol] {
						continue
					}
					if !matchesPerfKeyword(r.JobTypeName) {
						continue
					}
					if _, seen := found[r.JobGroupSymbol]; !seen {
						found[r.JobGroupSymbol] = r.JobTypeName
					}
				}
			}(repo, page)
		}
	}
	wg.Wait()

	out := make([]string, 0, len(found))
	for sym, example := range found {
		out = append(out, fmt.Sprintf("%s  (e.g. %s)", sym, example))
	}
	sort.Strings(out)
	return out
}

func matchesPerfKeyword(jobTypeName string) bool {
	n := strings.ToLower(jobTypeName)
	for _, kw := range perfJobGroupKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}
