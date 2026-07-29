package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeJobTypeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// Real Treeherder format: " <12 hex> <12 hex>" = 26 trailing chars.
			name: "side by side hashes collapsed",
			in:   "test-linux2404-64-shippable browsertime-tp6-firefox-youtube c4c52051e373 16732e03f66f",
			want: "test-linux2404-64-shippable browsertime-tp6-firefox-youtube-side-by-side",
		},
		{
			// Only one hash: not a side-by-side job, must be left alone.
			name: "single trailing hash untouched",
			in:   "test-linux2404-64-shippable browsertime-tp6-firefox-youtube c4c52051e373",
			want: "test-linux2404-64-shippable browsertime-tp6-firefox-youtube c4c52051e373",
		},
		{
			name: "ordinary job untouched",
			in:   "test-linux2404-64-shippable/opt-browsertime-tp6-firefox-imdb",
			want: "test-linux2404-64-shippable/opt-browsertime-tp6-firefox-imdb",
		},
		{
			// Ends in hex but is a normal suite name; must not be mangled.
			name: "short hex-looking suffix untouched",
			in:   "talos-abcdef123456",
			want: "talos-abcdef123456",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeJobTypeName(tc.in); got != tc.want {
				t.Errorf("normalizeJobTypeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSuiteKeyFromJobType(t *testing.T) {
	tests := []struct{ in, want string }{
		{"test-linux2404-64-shippable/opt-browsertime-tp6-firefox-imdb", "browsertime-tp6-firefox-imdb"},
		{"test-windows11-64-25h2/debug-talos-other", "talos-other"},
		{"test-macosx1470-64-shippable/pgo-awsy-base", "awsy-base"},
		// mozperftest job types carry no platform/variant prefix.
		{"perftest-android-hw-a55-background-resource-fenix", "perftest-android-hw-a55-background-resource-fenix"},
		{"build-macosx64-aarch64-shippable/opt", "build-macosx64-aarch64-shippable/opt"},
	}
	for _, tc := range tests {
		if got := suiteKeyFromJobType(tc.in); got != tc.want {
			t.Errorf("suiteKeyFromJobType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSuiteRatePercent(t *testing.T) {
	tests := []struct {
		total, success int
		want           string
	}{
		{total: 0, success: 0, want: ""},
		{total: 78, success: 9, want: "11.5%"},
		{total: 152, success: 148, want: "97.4%"},
		{total: 10, success: 10, want: "100.0%"},
		{total: 7, success: 0, want: "0.0%"},
	}
	for _, tc := range tests {
		got := SuiteRate{Total: tc.total, Success: tc.success}.Percent()
		if got != tc.want {
			t.Errorf("SuiteRate{%d/%d}.Percent() = %q, want %q", tc.success, tc.total, got, tc.want)
		}
	}
}

func TestSortJobStats(t *testing.T) {
	stats := []JobStat{
		{JobTypeName: "b-mid", Total: 100, Success: 90},
		{JobTypeName: "c-perfect", Total: 100, Success: 100},
		{JobTypeName: "a-worst-small", Total: 3, Success: 0},
		{JobTypeName: "a-worst-big", Total: 200, Success: 0},
	}
	sortJobStats(stats)

	got := make([]string, len(stats))
	for i, s := range stats {
		got[i] = s.JobTypeName
	}
	// Worst rate first; among equal rates the higher-volume one ranks first.
	want := []string{"a-worst-big", "a-worst-small", "b-mid", "c-perfect"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortJobStats order = %v, want %v", got, want)
		}
	}
}

func TestSortJobStatsNoDivideByZero(t *testing.T) {
	// A zero-run entry must not panic or produce a non-deterministic order.
	stats := []JobStat{
		{JobTypeName: "empty", Total: 0, Success: 0},
		{JobTypeName: "real", Total: 10, Success: 5},
	}
	sortJobStats(stats)
	if len(stats) != 2 {
		t.Fatalf("lost entries during sort")
	}
}

func TestFilterJobStats(t *testing.T) {
	stats := []JobStat{
		{JobTypeName: "too-few-runs", Total: 5, Success: 0},
		{JobTypeName: "bad", Total: 100, Success: 40},
		{JobTypeName: "perfect", Total: 100, Success: 100},
		{JobTypeName: "slightly-bad", Total: 50, Success: 49},
	}
	got := filterJobStats(stats, 20, 0, false)

	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	for _, s := range got {
		if s.JobTypeName == "perfect" {
			t.Error("fully passing job type should be filtered out")
		}
		if s.JobTypeName == "too-few-runs" {
			t.Error("job type below minRuns should be filtered out")
		}
	}

	if limited := filterJobStats(stats, 20, 1, false); len(limited) != 1 {
		t.Errorf("limit=1 returned %d rows, want 1", len(limited))
	}
}

func TestFilterJobStatsExcludeAndroid(t *testing.T) {
	stats := []JobStat{
		{JobTypeName: "perftest-android-hw-a55-background-resource-fenix", Total: 78, Success: 9},
		{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Total: 100, Success: 80},
	}

	if got := filterJobStats(stats, 20, 0, false); len(got) != 2 {
		t.Errorf("android included: got %d rows, want 2", len(got))
	}

	got := filterJobStats(stats, 20, 0, true)
	if len(got) != 1 {
		t.Fatalf("android excluded: got %d rows, want 1", len(got))
	}
	if strings.Contains(got[0].JobTypeName, "android") {
		t.Errorf("android row survived exclusion: %q", got[0].JobTypeName)
	}
}

func TestExclusiveEnd(t *testing.T) {
	// The bound is exclusive, so it must be the next day at midnight -- using
	// 23:59:59 would silently drop jobs finishing in that final second.
	if got := exclusiveEnd("2026-07-28"); got != "2026-07-29T00:00:00" {
		t.Errorf("exclusiveEnd(2026-07-28) = %q, want 2026-07-29T00:00:00", got)
	}
	// Month/year rollover.
	if got := exclusiveEnd("2026-12-31"); got != "2027-01-01T00:00:00" {
		t.Errorf("exclusiveEnd(2026-12-31) = %q, want 2027-01-01T00:00:00", got)
	}
	// Unparseable input keeps a bound rather than dropping the filter.
	if got := exclusiveEnd("garbage"); got != "garbageT23:59:59" {
		t.Errorf("exclusiveEnd(garbage) = %q, want the end-of-day fallback", got)
	}
}

func TestSweepStatusWarning(t *testing.T) {
	if w := (SweepStatus{Total: 102, Failed: 0}).Warning(); w != "" {
		t.Errorf("clean sweep should have no warning, got %q", w)
	}
	s := SweepStatus{Total: 102, Failed: 3}
	if !s.Partial() {
		t.Error("sweep with failures should report Partial")
	}
	if w := s.Warning(); !strings.Contains(w, "3 of 102") {
		t.Errorf("warning should name the counts, got %q", w)
	}
}

func TestSuiteRatesFrom(t *testing.T) {
	// Two job types on different platforms roll up into one suite key.
	stats := []JobStat{
		{JobTypeName: "test-linux2404-64-shippable/opt-browsertime-tp6-firefox-imdb", Total: 100, Success: 98},
		{JobTypeName: "test-windows11-64-25h2-shippable/opt-browsertime-tp6-firefox-imdb", Total: 50, Success: 40},
		{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Total: 30, Success: 30},
	}
	rates := suiteRatesFrom(stats)

	imdb, ok := rates["browsertime-tp6-firefox-imdb"]
	if !ok {
		t.Fatalf("missing rolled-up suite key; got %v", rates)
	}
	if imdb.Total != 150 || imdb.Success != 138 {
		t.Errorf("imdb rollup = %d/%d, want 138/150", imdb.Success, imdb.Total)
	}
	if imdb.Percent() != "92.0%" {
		t.Errorf("imdb percent = %q, want %q", imdb.Percent(), "92.0%")
	}
}

func TestJoinSuiteRates(t *testing.T) {
	suites := []SuiteResult{
		{Suite: "browsertime-tp6-firefox-imdb"},
		{Suite: "suite-with-no-job-data"},
		{Suite: "zero-run-suite"},
	}
	rates := map[string]*SuiteRate{
		"browsertime-tp6-firefox-imdb": {Total: 150, Success: 138},
		"zero-run-suite":               {Total: 0, Success: 0},
	}

	matched := joinSuiteRates(suites, rates)
	if matched != 1 {
		t.Errorf("matched = %d, want 1", matched)
	}
	if suites[0].SuccessRate != "92.0%" || suites[0].JobRuns != 150 {
		t.Errorf("joined suite = %q/%d runs, want 92.0%%/150", suites[0].SuccessRate, suites[0].JobRuns)
	}
	// An unmatched suite must stay empty so the template can distinguish
	// "unknown" from "100% pass".
	if suites[1].SuccessRate != "" {
		t.Errorf("unmatched suite got rate %q, want empty", suites[1].SuccessRate)
	}
	if suites[2].SuccessRate != "" {
		t.Errorf("zero-run suite got rate %q, want empty", suites[2].SuccessRate)
	}
}

// fakeJobsServer serves paginated /jobs/ responses so pagination can be tested
// without hitting Treeherder.
func fakeJobsServer(t *testing.T, jobsPerGroup map[string][]THJob, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			requests.Add(1)
		}
		q := r.URL.Query()
		group := q.Get("job_group_symbol")
		offset, _ := strconv.Atoi(q.Get("offset"))
		count, _ := strconv.Atoi(q.Get("count"))

		all := jobsPerGroup[group]
		var page []THJob
		if offset < len(all) {
			end := offset + count
			if end > len(all) {
				end = len(all)
			}
			page = all[offset:end]
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"results": page}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
}

func TestFetchJobStatsTalliesAndJoins(t *testing.T) {
	jobs := []THJob{
		{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Result: "success", Tier: 1},
		{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Result: "success", Tier: 1},
		{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Result: "testfailed", Tier: 1},
		{JobTypeName: "perftest-android-hw-a55-background-resource-fenix", Result: "testfailed", Tier: 2},
	}
	server := fakeJobsServer(t, map[string][]THJob{"T": jobs}, nil)
	defer server.Close()

	oldBase, oldGroups, oldConc := treeherderBase, perfJobGroups, maxConcurrent
	treeherderBase, perfJobGroups, maxConcurrent = server.URL, []string{"T"}, 4
	t.Cleanup(func() { treeherderBase, perfJobGroups, maxConcurrent = oldBase, oldGroups, oldConc })

	stats, sweep := fetchJobStats("2026-07-21", "2026-07-28")
	if sweep.Partial() {
		t.Errorf("clean sweep reported partial: %+v", sweep)
	}

	// Every repo hits the same mock, so counts are multiplied by len(perfRepos).
	repos := len(perfRepos)
	byName := map[string]JobStat{}
	for _, s := range stats {
		byName[s.JobTypeName] = s
	}

	talos, ok := byName["test-linux2404-64-shippable/opt-talos-other"]
	if !ok {
		t.Fatalf("missing talos stat, got %+v", stats)
	}
	if talos.Total != 3*repos || talos.Success != 2*repos {
		t.Errorf("talos = %d/%d, want %d/%d", talos.Success, talos.Total, 2*repos, 3*repos)
	}
	if talos.Rate != "66.7%" {
		t.Errorf("talos rate = %q, want 66.7%%", talos.Rate)
	}

	perf, ok := byName["perftest-android-hw-a55-background-resource-fenix"]
	if !ok {
		t.Fatal("missing mozperftest stat")
	}
	if perf.Tier != 2 {
		t.Errorf("tier = %d, want 2", perf.Tier)
	}
	if perf.Rate != "0.0%" {
		t.Errorf("perftest rate = %q, want 0.0%%", perf.Rate)
	}

	// Worst rate must sort first.
	if stats[0].JobTypeName != "perftest-android-hw-a55-background-resource-fenix" {
		t.Errorf("first stat = %q, want the 0%% job", stats[0].JobTypeName)
	}
}

func TestFetchJobGroupPaginates(t *testing.T) {
	// One full page plus a partial page: the fetcher must request both and stop.
	jobs := make([]THJob, jobsPageSize+5)
	for i := range jobs {
		jobs[i] = THJob{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Result: "success"}
	}
	var requests atomic.Int32
	server := fakeJobsServer(t, map[string][]THJob{"T": jobs}, &requests)
	defer server.Close()

	oldBase := treeherderBase
	treeherderBase = server.URL
	t.Cleanup(func() { treeherderBase = oldBase })

	got, complete := fetchJobGroup("autoland", "T", "2026-07-21", "2026-07-28")

	if !complete {
		t.Error("a fully-fetched sweep should report complete")
	}
	if len(got) != jobsPageSize+5 {
		t.Errorf("got %d jobs, want %d", len(got), jobsPageSize+5)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("made %d requests, want 2 (one full page + one partial)", n)
	}
}

// A page failing partway through pagination must NOT yield a partial tally.
// Truncation shrinks the denominator while failures survive, so a partial
// sweep does not read as "incomplete", it reads as a much worse success rate.
func TestFetchJobGroupDiscardsTruncatedSweep(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n > 1 {
			// Second page and beyond: persistent 500 exhausts the retries.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		page := make([]THJob, jobsPageSize)
		for i := range page {
			page[i] = THJob{JobTypeName: "test-linux2404-64-shippable/opt-talos-other", Result: "success"}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"results": page}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer server.Close()

	oldBase, oldSleep := treeherderBase, retrySleep
	treeherderBase = server.URL
	retrySleep = func(time.Duration) {}
	t.Cleanup(func() { treeherderBase, retrySleep = oldBase, oldSleep })

	got, complete := fetchJobGroup("autoland", "T", "2026-07-21", "2026-07-28")

	if complete {
		t.Error("sweep that failed mid-pagination must not report complete")
	}
	if got != nil {
		t.Errorf("failed sweep must discard its rows, got %d", len(got))
	}
}

func TestFetchJobStatsReportsPartialSweep(t *testing.T) {
	// Every request fails, so every repo/group sweep is dropped.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldBase, oldGroups, oldSleep := treeherderBase, perfJobGroups, retrySleep
	treeherderBase, perfJobGroups = server.URL, []string{"T", "Btime"}
	retrySleep = func(time.Duration) {}
	t.Cleanup(func() { treeherderBase, perfJobGroups, retrySleep = oldBase, oldGroups, oldSleep })

	stats, sweep := fetchJobStats("2026-07-21", "2026-07-28")

	if len(stats) != 0 {
		t.Errorf("all sweeps failed but got %d stats", len(stats))
	}
	want := len(perfRepos) * 2
	if sweep.Total != want || sweep.Failed != want {
		t.Errorf("sweep = %d/%d failed, want %d/%d", sweep.Failed, sweep.Total, want, want)
	}
	if !sweep.Partial() || sweep.Warning() == "" {
		t.Error("fully failed sweep must surface a warning")
	}
}

func TestFetchJobsPageSendsWindowAndGroup(t *testing.T) {
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"results":[]}`)
	}))
	defer server.Close()

	oldBase := treeherderBase
	treeherderBase = server.URL
	t.Cleanup(func() { treeherderBase = oldBase })

	fetchJobsPage("autoland", "Btime", "2026-07-21", "2026-07-28", 3)

	for _, want := range []string{
		"/project/autoland/jobs/",
		"job_group_symbol=Btime",
		"end_time__gt=2026-07-21T00%3A00%3A00",
		// Exclusive upper bound = start of the next day, so the whole of the
		// final day is included (23:59:59 would drop that last second).
		"end_time__lt=2026-07-29T00%3A00%3A00",
		"offset=" + strconv.Itoa(3*jobsPageSize),
	} {
		if !strings.Contains(gotURL, want) {
			t.Errorf("request URL %q missing %q", gotURL, want)
		}
	}
}

func TestFetchJobsPageHandlesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	oldBase := treeherderBase
	treeherderBase = server.URL
	t.Cleanup(func() { treeherderBase = oldBase })

	jobs, more, ok := fetchJobsPage("autoland", "T", "2026-07-21", "2026-07-28", 0)
	if len(jobs) != 0 || more {
		t.Errorf("bad response should yield no jobs and no continuation, got %d jobs more=%v", len(jobs), more)
	}
	// ok must be false so the caller can tell failure from an empty page.
	if ok {
		t.Error("failed page must report ok=false, not an empty success")
	}
}
