# Improvements Roadmap

Planned improvements in implementation order. Each step is a self-contained commit.
Status: [ ] pending | [x] done | [~] in progress

---

## Step 1 — Retry with backoff
**Status:** [x]

Add retry logic (3 attempts, exponential backoff) to the `get()` helper for transient
failures (5xx, timeouts). Currently a single transient Treeherder/Bugzilla error crashes
the whole run.

**Changes:**
- `get()` in `main.go`: wrap with retry loop, backoff on 5xx or timeout errors
- Add test: mock server that fails N times then succeeds

---

## Step 2 — Copy-to-clipboard button
**Status:** [ ] skipped

Add a button to the HTML report that copies a plain-text summary of all bugs to the
clipboard. Useful for pasting into the internal Google Doc during triage.

**Format:**
```
🟧 Intermittent Failures

Raptor
- Bug 1234 - Summary (142 failures) [autoland: 120, mozilla-central: 22]
- Bug 5678 - Summary (80 failures)

🟥 Perma Failures
...
```

**Changes:**
- `template.html`: add button + JS `navigator.clipboard.writeText()` logic
- Builds plain text from the DOM at click time, no Go changes needed

---

## Step 3 — Bug age
**Status:** [x]

Show how long a bug has been open. Helps triage prioritise long-running unfixed
intermittents.

**Display:** `Opened: 2024-11-03 (137 days ago)`

**Changes:**
- Add `creation_time` to Bugzilla `include_fields` query in both fetchers
- Add `CreationTime string` to `Bug`, `Result`, `PermaBug`
- Compute age in days at report generation time
- Add `Age string` field to `Result`/`PermaBug`, render in template
- Add test: verify age calculation

---

## Step 4 — Top test suites
**Status:** [~] stashed — on branch `feature/test-suite-breakdown`, pending team input

> **Partly overtaken by Step 8.** The Test Suites tab answers the inverse question:
> it pivots suite → contributing bugs, where this branch pivots bug → suites. If the
> tab proves sufficient in triage, drop the branch; it has drifted 17 commits behind
> main and conflicts with the 2d-window work. Note `suiteBreakdownFrom()` on main
> already does the per-bug suite counting this branch adds, minus the sort by count.

The `test_suite` field from `THJobFailure` is already fetched but unused. Surface the
top failing test suites per bug (e.g. `raptor-tp6: 45, raptor-speedometer: 12`).

**Changes:**
- `aggregateBreakdown()`: count by `test_suite`, return top N (e.g. 5)
- Add `TestSuites []string` to `Result` and `PermaBug`
- Render in template under breakdown section
- Update `TestAggregateBreakdown` test

---

## Step 5 — Intermittent rate
**Status:** [x]

Show failure rate (failures/total pushes) rather than just raw count. A bug with 50
failures in 100 pushes is more severe than 50 failures in 1000 pushes.

**Display:** `142 failures (8.3% rate)`

**New API call:** `/api/failurecount/?startday=...&endday=...&tree=all&bug=ID`
Returns `[{date, test_runs, failure_count}]` — sum both fields to get totals.

**Changes:**
- Add `THDailyCount` struct: `{ Date string, TestRuns int, FailureCount int }`
- Add `fetchFailureRate(bugID int, start, end string) (failures int, rate float64)`
- Replace `fetchTreeherderCounts` bulk call with per-bug `failurecount` call
  (or keep bulk for threshold filtering, add rate as enrichment step)
- Add `Rate float64` to `Result`, render in template
- Add test for rate calculation

**Note:** The bulk `/api/failures/` call is still useful for fast threshold filtering.
Add rate as a separate enrichment call only for qualifying bugs.

---

## Step 6 — "New this week" badge + Trend detection
**Status:** [x]

Two related features sharing one extra API call (previous period's failure counts).

**"New this week":** bug had 0 failures in the prior window but >0 this week → show 🆕 badge.

**Trend:** compare this week's count to last week's → show `↑ +45` or `↓ -20` next to
failure count.

**Changes:**
- Fetch previous period counts: `fetchTreeherderCounts(prevStart, prevEnd)`
  where `prevStart = startDay - DaysBack`, `prevEnd = startDay`
- Add `PrevFailures int` to `Result`
- Compute `Trend string` (e.g. `↑ +45`, `↓ -20`, `🆕`) at result-building time
- Render in template next to failure count
- Add tests for trend computation logic

---

## Step 7 — Historical report archiving
**Status:** [ ] skipped — low value if nobody reviews past reports

Keep dated copies of past reports on GitHub Pages instead of always overwriting
`index.html`. Enables manual trend review over time.

**Storage cost:** ~30-50KB/report × 365 days ≈ ~15MB/year. Well within GH Pages 1GB limit.

**Changes:**
- `daily-report.yml`: after generating `report.html`, also write `report-YYYY-MM-DD.html`
  and commit both to `gh-pages`
- Generate a simple `archive.html` index listing past reports by date
- Update `daily-report.yml` to use `keep_files: true` in `peaceiris/actions-gh-pages`
  so old reports aren't deleted on each push

---

## Step 8 — Merge the suite report in as a tab + per-job success rate
**Status:** [x]

Folded the standalone `perftest-suite-report` tool into this one as a second tab, and
added a third tab reproducing the perf Redash dashboard's per-job success percentage.

**Changes:**
- `suites.go`: the ported suite report, reusing this repo's `get()`,
  `normalizePlatform()`, `fetchRawBreakdown()`, `Bug` and `THJobFailure`
- `jobs.go`: per-job success rate over `/api/project/{repo}/jobs/`
- `template.html`: three tab panels, deep-linkable via `#tab-…`
- `writeHTMLReport` takes a `reportInput` struct so future tabs don't churn callers

**Findings worth remembering** (see README "The two different rates"):
- The Treeherder jobs API **silently ignores unknown filter params**. `job_type_name__contains`
  and `test_suite` are accepted and not applied. Verify any new filter actually constrains
  the result before trusting it.
- `test_runs` in `/api/failurecount/` is **pushes**, not job runs — so the pre-existing
  per-bug "rate" was never a per-job rate.
- Perf jobs are selected by enumerating `job_group_symbol`; the set is open-ended.
  `--discover-job-groups` reports drift.

**Follow-ups:**
- Improve the `test_suite` ↔ `job_type_name` join (currently ~2/3 of suites match)
- Consider whether to exclude android to match the dashboard exactly

---

## Implementation Notes

- Each step is independently committable
- Run `go test ./...` + `golangci-lint run` before each commit
- Steps 5 and 6 both add Treeherder API calls — be mindful of rate limiting
- Steps 3-6 require template changes — keep `template.html` clean
