package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// ===================== Test suite report =====================
//
// Ported from the standalone perftest-suite-report tool. Where that tool had
// its own copy of a helper, this uses the one already in main.go: get(),
// normalizePlatform(), fetchRawBreakdown(), Bug, THJobFailure, renderHTML().

// SuiteBugContribution is one bug's share of a suite's failures.
type SuiteBugContribution struct {
	ID        int
	Link      string
	GraphLink string
	Summary   string
	Component string
	Failures  int
}

// SuiteResult is one test suite's failure profile over the window.
type SuiteResult struct {
	Rank          int
	Suite         string
	TotalFailures int
	TwoDayFails   int
	Spike         bool // 2d share exceeds 1.5x the expected 2/7 rate
	Platforms     []string
	Trees         []string
	Bugs          []SuiteBugContribution

	// Populated by joinSuiteRates from the per-job success data. Empty when
	// the suite has no matching job type (see joinSuiteRates for why).
	SuccessRate string
	JobRuns     int
}

// HarnessGroup buckets suites by the harness that runs them.
type HarnessGroup struct {
	Name          string
	TotalFailures int
	TwoDayFails   int
	Suites        []SuiteResult
}

// suiteAgg accumulates per-suite counts while fanning out over bugs.
type suiteAgg struct {
	bugs      map[int]int
	platforms map[string]int
	trees     map[string]int
}

// fetchSuiteBugs returns the perf bugs the suite report aggregates over: open
// intermittent-failure bugs in the perf components, plus the Perma subset.
//
// This deliberately does NOT reuse fetchIntermittentBugs, which filters out
// summaries containing "perma" for the triage tab. The suite view wants
// permafails included, since they are often the top suite contributors.
func fetchSuiteBugs() []Bug {
	baseParams := func() url.Values {
		p := url.Values{}
		p.Set("product", "Testing")
		p.Set("resolution", "---")
		p.Set("keywords", "intermittent-failure")
		p.Set("keywords_type", "allwords")
		p.Set("include_fields", "id,summary,component")
		for _, c := range components {
			p.Add("component", c)
		}
		return p
	}

	withPerma := baseParams()
	withPerma.Set("short_desc", "Perma")
	withPerma.Set("short_desc_type", "allwordssubstr")

	queries := []url.Values{baseParams(), withPerma}
	results := make([][]Bug, len(queries))

	var wg sync.WaitGroup
	for i, params := range queries {
		wg.Add(1)
		go func(i int, params url.Values) {
			defer wg.Done()
			results[i] = fetchBugList(params)
		}(i, params)
	}
	wg.Wait()

	seen := map[int]bool{}
	var deduped []Bug
	for _, batch := range results {
		for _, b := range batch {
			if !seen[b.ID] {
				seen[b.ID] = true
				deduped = append(deduped, b)
			}
		}
	}
	return deduped
}

// aggregateBySuite fans out over bugs and tallies failures per test suite.
func aggregateBySuite(bugs []Bug, start, end string) map[string]*suiteAgg {
	var mu sync.Mutex
	var wg sync.WaitGroup
	result := map[string]*suiteAgg{}

	for _, b := range bugs {
		wg.Add(1)
		go func(bug Bug) {
			defer wg.Done()
			acquire()
			defer release()

			failures := fetchRawBreakdown(bug.ID, start, end)
			if len(failures) == 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, f := range failures {
				if f.TestSuite == "" {
					continue
				}
				agg, ok := result[f.TestSuite]
				if !ok {
					agg = &suiteAgg{
						bugs:      map[int]int{},
						platforms: map[string]int{},
						trees:     map[string]int{},
					}
					result[f.TestSuite] = agg
				}
				agg.bugs[bug.ID]++
				if p := normalizePlatform(f.Platform); p != "" {
					agg.platforms[p]++
				}
				if f.Tree != "" {
					agg.trees[f.Tree]++
				}
			}
		}(b)
	}
	wg.Wait()
	return result
}

// sortedCountStrs renders a count map as "name: count", highest first.
func sortedCountStrs(m map[string]int) []string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = fmt.Sprintf("%s: %d", p.k, p.v)
	}
	return out
}

func classifyHarness(suite string) string {
	s := strings.ToLower(suite)
	switch {
	case strings.HasPrefix(s, "raptor-"), strings.HasPrefix(s, "browsertime-"):
		return "Raptor"
	case strings.HasPrefix(s, "talos-"):
		return "Talos"
	case strings.HasPrefix(s, "awsy-"):
		return "AWSY"
	case strings.HasPrefix(s, "perftest-"), strings.HasPrefix(s, "mozperftest-"):
		return "mozperftest"
	default:
		return "Other"
	}
}

// isSpike reports whether the 2-day failure share exceeds 1.5x the expected
// 2/7 rate, i.e. twoDayFails/totalFailures > 3/7. Integer math, no floats.
func isSpike(totalFailures, twoDayFails int) bool {
	if totalFailures == 0 {
		return false
	}
	return twoDayFails*7 > totalFailures*3
}

// buildSuiteResults ranks suites by failure count and truncates to topN.
func buildSuiteResults(bugs []Bug, current, twoday map[string]*suiteAgg, start, end string) []SuiteResult {
	bugByID := make(map[int]Bug, len(bugs))
	for _, b := range bugs {
		bugByID[b.ID] = b
	}

	results := make([]SuiteResult, 0, len(current))
	for suite, data := range current {
		total := 0
		for _, c := range data.bugs {
			total += c
		}

		twoDayTotal := 0
		if td, ok := twoday[suite]; ok {
			for _, c := range td.bugs {
				twoDayTotal += c
			}
		}

		type bugCount struct{ id, count int }
		counts := make([]bugCount, 0, len(data.bugs))
		for id, c := range data.bugs {
			counts = append(counts, bugCount{id, c})
		}
		sort.Slice(counts, func(i, j int) bool {
			if counts[i].count != counts[j].count {
				return counts[i].count > counts[j].count
			}
			return counts[i].id < counts[j].id
		})

		contributions := make([]SuiteBugContribution, 0, len(counts))
		for _, c := range counts {
			bug := bugByID[c.id]
			contributions = append(contributions, SuiteBugContribution{
				ID:        c.id,
				Link:      fmt.Sprintf("https://bugzilla.mozilla.org/show_bug.cgi?id=%d", c.id),
				GraphLink: fmt.Sprintf("https://treeherder.mozilla.org/intermittent-failures/bugdetails?startday=%s&endday=%s&tree=all&bug=%d", start, end, c.id),
				Summary:   bug.Summary,
				Component: bug.Component,
				Failures:  c.count,
			})
		}

		results = append(results, SuiteResult{
			Suite:         suite,
			TotalFailures: total,
			TwoDayFails:   twoDayTotal,
			Spike:         isSpike(total, twoDayTotal),
			Platforms:     sortedCountStrs(data.platforms),
			Trees:         sortedCountStrs(data.trees),
			Bugs:          contributions,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].TotalFailures != results[j].TotalFailures {
			return results[i].TotalFailures > results[j].TotalFailures
		}
		return results[i].Suite < results[j].Suite
	})
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results
}

// joinSuiteRates attaches per-job success rates to ranked suites.
//
// The two data sources are keyed differently: the suite report keys on
// Treeherder's `test_suite` from /failuresbybug/, while job stats key on
// `job_type_name` from the jobs API. suiteKeyFromJobType bridges them by
// stripping the "test-<platform>/<variant>-" prefix. The join is not total --
// a suite whose jobs live in a job group missing from perfJobGroups will not
// match -- so callers must treat an empty SuccessRate as "unknown", not 100%.
func joinSuiteRates(suites []SuiteResult, rates map[string]*SuiteRate) (matched int) {
	for i := range suites {
		r, ok := rates[suites[i].Suite]
		if !ok || r.Total == 0 {
			continue
		}
		suites[i].SuccessRate = r.Percent()
		suites[i].JobRuns = r.Total
		matched++
	}
	return matched
}

// groupByHarness groups a ranked suite list into harness sections. Ranks are
// renumbered within each group; groups are ordered by combined failure count.
func groupByHarness(suites []SuiteResult) []HarnessGroup {
	order := []string{}
	m := map[string]*HarnessGroup{}
	for i := range suites {
		h := classifyHarness(suites[i].Suite)
		g, ok := m[h]
		if !ok {
			g = &HarnessGroup{Name: h}
			m[h] = g
			order = append(order, h)
		}
		g.Suites = append(g.Suites, suites[i])
		g.TotalFailures += suites[i].TotalFailures
		g.TwoDayFails += suites[i].TwoDayFails
	}

	groups := make([]HarnessGroup, 0, len(m))
	for _, name := range order {
		g := m[name]
		for i := range g.Suites {
			g.Suites[i].Rank = i + 1
		}
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].TotalFailures != groups[j].TotalFailures {
			return groups[i].TotalFailures > groups[j].TotalFailures
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

// analyzeSuites runs the full suite pipeline for both windows.
func analyzeSuites(bugs []Bug, start, end, twoDayStart string) []SuiteResult {
	if len(bugs) == 0 {
		return nil
	}

	var current, twoDay map[string]*suiteAgg
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); current = aggregateBySuite(bugs, start, end) }()
	go func() { defer wg.Done(); twoDay = aggregateBySuite(bugs, twoDayStart, end) }()
	wg.Wait()

	return buildSuiteResults(bugs, current, twoDay, start, end)
}
