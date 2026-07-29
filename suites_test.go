package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClassifyHarness(t *testing.T) {
	tests := []struct{ suite, want string }{
		{"raptor-tp6-cold-fenix-firefox", "Raptor"},
		{"browsertime-tp6-firefox-imdb", "Raptor"},
		{"talos-damp-inspector", "Talos"},
		{"awsy-base", "AWSY"},
		{"perftest-android-hw-a55-background-resource-fenix", "mozperftest"},
		{"mozperftest-unittest", "mozperftest"},
		{"mochitest-plain-2", "Other"},
		{"TALOS-OTHER", "Talos"},
		{"", "Other"},
	}
	for _, tc := range tests {
		if got := classifyHarness(tc.suite); got != tc.want {
			t.Errorf("classifyHarness(%q) = %q, want %q", tc.suite, got, tc.want)
		}
	}
}

func TestIsSpike(t *testing.T) {
	tests := []struct {
		name          string
		total, twoDay int
		want          bool
	}{
		{"no failures", 0, 0, false},
		{"even spread over 7d", 70, 20, false},  // 2/7 == expected, not a spike
		{"clearly spiking", 100, 90, true},      // 90% of failures in last 2 days
		{"exactly at threshold", 70, 30, false}, // 30*7 == 70*3, strict >
		{"just over threshold", 70, 31, true},
		{"all recent", 10, 10, true},
	}
	for _, tc := range tests {
		if got := isSpike(tc.total, tc.twoDay); got != tc.want {
			t.Errorf("%s: isSpike(%d, %d) = %v, want %v", tc.name, tc.total, tc.twoDay, got, tc.want)
		}
	}
}

func TestSortedCountStrs(t *testing.T) {
	got := sortedCountStrs(map[string]int{"linux1804": 3, "windows11": 10, "macosx1470": 3})
	want := []string{"windows11: 10", "linux1804: 3", "macosx1470: 3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			// Ties must break alphabetically so output is stable across runs.
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(sortedCountStrs(map[string]int{})) != 0 {
		t.Error("empty map should give empty slice")
	}
}

func TestBuildSuiteResultsRanksAndTruncates(t *testing.T) {
	oldTop := topN
	topN = 2
	t.Cleanup(func() { topN = oldTop })

	bugs := []Bug{
		{ID: 1, Summary: "bug one", Component: "Raptor"},
		{ID: 2, Summary: "bug two", Component: "Talos"},
	}
	current := map[string]*suiteAgg{
		"talos-other": {
			bugs:      map[int]int{2: 5},
			platforms: map[string]int{"linux1804": 5},
			trees:     map[string]int{"autoland": 5},
		},
		"browsertime-tp6-firefox-imdb": {
			bugs:      map[int]int{1: 20, 2: 3},
			platforms: map[string]int{"windows11": 23},
			trees:     map[string]int{"autoland": 23},
		},
		"awsy-base": {
			bugs:      map[int]int{1: 1},
			platforms: map[string]int{},
			trees:     map[string]int{},
		},
	}
	twoDay := map[string]*suiteAgg{
		"browsertime-tp6-firefox-imdb": {bugs: map[int]int{1: 15}},
	}

	got := buildSuiteResults(bugs, current, twoDay, "2026-07-21", "2026-07-28")

	if len(got) != 2 {
		t.Fatalf("got %d suites, want 2 (topN)", len(got))
	}
	if got[0].Suite != "browsertime-tp6-firefox-imdb" || got[0].TotalFailures != 23 {
		t.Errorf("top suite = %q with %d failures, want browsertime-tp6-firefox-imdb with 23",
			got[0].Suite, got[0].TotalFailures)
	}
	if got[0].Rank != 1 || got[1].Rank != 2 {
		t.Errorf("ranks = %d,%d want 1,2", got[0].Rank, got[1].Rank)
	}
	if got[0].TwoDayFails != 15 || !got[0].Spike {
		t.Errorf("2d = %d spike = %v, want 15 and spiking", got[0].TwoDayFails, got[0].Spike)
	}
	// Contributing bugs are ordered by failure count, highest first.
	if len(got[0].Bugs) != 2 || got[0].Bugs[0].ID != 1 || got[0].Bugs[0].Failures != 20 {
		t.Errorf("contributions = %+v, want bug 1 (20) first", got[0].Bugs)
	}
	if got[0].Bugs[0].Summary != "bug one" || got[0].Bugs[0].Component != "Raptor" {
		t.Errorf("bug metadata not joined: %+v", got[0].Bugs[0])
	}
	if !strings.Contains(got[0].Bugs[0].GraphLink, "bug=1") {
		t.Errorf("graph link missing bug id: %q", got[0].Bugs[0].GraphLink)
	}
}

func TestGroupByHarnessRenumbersAndOrders(t *testing.T) {
	suites := []SuiteResult{
		{Rank: 1, Suite: "browsertime-a", TotalFailures: 50, TwoDayFails: 10},
		{Rank: 2, Suite: "talos-a", TotalFailures: 30},
		{Rank: 3, Suite: "browsertime-b", TotalFailures: 20},
		{Rank: 4, Suite: "talos-b", TotalFailures: 5},
	}
	groups := groupByHarness(suites)

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0].Name != "Raptor" || groups[0].TotalFailures != 70 {
		t.Errorf("first group = %q (%d), want Raptor (70)", groups[0].Name, groups[0].TotalFailures)
	}
	if groups[0].TwoDayFails != 10 {
		t.Errorf("group 2d total = %d, want 10", groups[0].TwoDayFails)
	}
	if groups[1].Name != "Talos" || groups[1].TotalFailures != 35 {
		t.Errorf("second group = %q (%d), want Talos (35)", groups[1].Name, groups[1].TotalFailures)
	}
	// Rank is renumbered within each group.
	for _, g := range groups {
		for i, s := range g.Suites {
			if s.Rank != i+1 {
				t.Errorf("group %s suite %q rank = %d, want %d", g.Name, s.Suite, s.Rank, i+1)
			}
		}
	}
}

func TestGroupByHarnessEmpty(t *testing.T) {
	if got := groupByHarness(nil); len(got) != 0 {
		t.Errorf("groupByHarness(nil) = %v, want empty", got)
	}
}

func TestAggregateBySuite(t *testing.T) {
	failures := []THJobFailure{
		{Platform: "linux1804-64-shippable-qr", Tree: "autoland", TestSuite: "talos-other"},
		{Platform: "linux1804-64-shippable-qr", Tree: "autoland", TestSuite: "talos-other"},
		{Platform: "windows11-64-25h2", Tree: "mozilla-central", TestSuite: "awsy-base"},
		{Platform: "linux1804", Tree: "autoland", TestSuite: ""}, // dropped: no suite
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(failures); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer server.Close()

	oldBase := treeherderBase
	treeherderBase = server.URL
	t.Cleanup(func() { treeherderBase = oldBase })

	got := aggregateBySuite([]Bug{{ID: 42}}, "2026-07-21", "2026-07-28")

	if len(got) != 2 {
		t.Fatalf("got %d suites, want 2 (blank suite dropped): %v", len(got), got)
	}
	talos := got["talos-other"]
	if talos == nil || talos.bugs[42] != 2 {
		t.Fatalf("talos-other bug count = %v, want 2", talos)
	}
	if talos.platforms["linux1804"] != 2 {
		t.Errorf("platform normalized count = %d, want 2", talos.platforms["linux1804"])
	}
	if talos.trees["autoland"] != 2 {
		t.Errorf("tree count = %d, want 2", talos.trees["autoland"])
	}
}

func TestFetchSuiteBugsDedupes(t *testing.T) {
	// Both Bugzilla queries return overlapping bugs; the union must be deduped.
	// The two requests are concurrent, so the counter must be atomic.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		payload := BugListResponse{Bugs: []Bug{{ID: 1, Summary: "shared"}}}
		if r.URL.Query().Get("short_desc") == "Perma" {
			payload.Bugs = append(payload.Bugs, Bug{ID: 2, Summary: "Perma only"})
		} else {
			payload.Bugs = append(payload.Bugs, Bug{ID: 3, Summary: "intermittent only"})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer server.Close()

	oldBase := bugzillaBase
	bugzillaBase = server.URL
	t.Cleanup(func() { bugzillaBase = oldBase })

	got := fetchSuiteBugs()

	if n := calls.Load(); n != 2 {
		t.Errorf("made %d Bugzilla calls, want 2", n)
	}
	if len(got) != 3 {
		t.Fatalf("got %d bugs, want 3 deduped: %+v", len(got), got)
	}
	seen := map[int]bool{}
	for _, b := range got {
		if seen[b.ID] {
			t.Errorf("duplicate bug %d", b.ID)
		}
		seen[b.ID] = true
	}
	// Perma bugs must be present: they are usually the top suite contributors.
	if !seen[2] {
		t.Error("perma bug missing from suite bug set")
	}
}

func TestAnalyzeSuitesEmptyBugs(t *testing.T) {
	if got := analyzeSuites(nil, "2026-07-21", "2026-07-28", "2026-07-26"); got != nil {
		t.Errorf("analyzeSuites(nil) = %v, want nil", got)
	}
}

func TestRenderHTMLTabs(t *testing.T) {
	data := reportData{
		Intermittents: groupByComponent([]Result{{
			ID: 1234, Summary: "Intermittent raptor timeout", Component: "Raptor", NumberFailures: 42,
			Link: "https://bugzilla.mozilla.org/show_bug.cgi?id=1234",
		}}, components),
		Harnesses: []HarnessGroup{{
			Name: "Raptor", TotalFailures: 23, TwoDayFails: 15,
			Suites: []SuiteResult{{
				Rank: 1, Suite: "browsertime-tp6-firefox-imdb", TotalFailures: 23,
				TwoDayFails: 15, Spike: true, SuccessRate: "92.0%", JobRuns: 150,
				Platforms: []string{"windows11: 23"},
				Bugs: []SuiteBugContribution{{
					ID: 1, Link: "https://bugzilla.mozilla.org/show_bug.cgi?id=1",
					Summary: "contributing bug", Failures: 20,
				}},
			}},
		}},
		JobStats: []JobStat{
			{JobTypeName: "perftest-android-hw-a55-background-resource-fenix", Tier: 2, Total: 78, Success: 9, Rate: "11.5%"},
			{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Tier: 1, Total: 100, Success: 95, Rate: "95.0%"},
		},
		Generated: "2026-07-28 09:00 UTC",
		DaysBack:  7,
		TopN:      25,
		MinRuns:   20,
	}

	var buf strings.Builder
	if err := renderHTML(&buf, reportTemplate, data); err != nil {
		t.Fatalf("renderHTML failed: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		// Tab scaffolding
		`id="tab-triage"`, `id="tab-suites"`, `id="tab-rates"`,
		`role="tablist"`, `aria-controls="tab-suites"`,
		// Triage tab content still renders
		"Bug 1234", "Intermittent raptor timeout",
		// Suite tab content
		"browsertime-tp6-firefox-imdb", "92.0% pass", "🔺 spiking", "contributing bug",
		// Rate tab content
		"perftest-android-hw-a55-background-resource-fenix", "11.5%",
		// ratePctClass funcmap applied: 9/78 is well under 90%
		`class="pct-bad"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("expected %q in HTML output", want)
		}
	}

	// Only the first tab is visible on load; the other two carry `hidden` so
	// the report is readable even with JavaScript disabled.
	if !strings.Contains(html, `<div class="tabpanel" id="tab-triage" role="tabpanel">`) {
		t.Error("triage panel should be visible on load (no hidden attribute)")
	}
	for _, id := range []string{"tab-suites", "tab-rates"} {
		want := `<div class="tabpanel" id="` + id + `" role="tabpanel" hidden>`
		if !strings.Contains(html, want) {
			t.Errorf("panel %s should start hidden; missing %q", id, want)
		}
	}
}

func TestRenderHTMLEmptyTabs(t *testing.T) {
	// A report with no suite or rate data must still produce valid tabs
	// rather than blank panels.
	var buf strings.Builder
	data := reportData{Generated: "2026-07-28 09:00 UTC", DaysBack: 7}
	if err := renderHTML(&buf, reportTemplate, data); err != nil {
		t.Fatalf("renderHTML failed: %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		"No suite failures found",
		"No job success data",
		`id="tab-suites"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("expected %q in HTML output", want)
		}
	}
}
