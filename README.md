# Perftest Triage Report Generator

A Go CLI tool that automates the generation of weekly performance test triage reports by querying Bugzilla and Treeherder for intermittent failures, perma failures, and generic task timeouts affecting perf tests.

Useful for perftest triage sessions where engineers need a concise and accurate snapshot of the week's flakiest or most problematic bugs.

The report is a single self-contained `report.html` with three tabs.

---

## Tabs

### 🟧 Intermittent Triage

Bug-centric. Grouped by component: AWSY, Condprofile, mozperftest, Performance, Raptor, Talos.

- **Intermittent Failures** — open bugs with the `intermittent-failure` keyword, filtered to those meeting the failure threshold
- **Perma Failures** — open bugs with "Perma" in the title, active in the report window
- **Generic Task Timeout** — perf-test failures (browsertime, talos, perftest, awsy) from [Bug 1809667](https://bugzilla.mozilla.org/show_bug.cgi?id=1809667), reported separately when they meet the failure threshold

### 🧪 Test Suites

Suite-centric view of the same failure data: the top N test suites ranked by failure count, grouped by harness, with platform/repository breakdowns and the bugs contributing to each. Flags suites whose failures are disproportionately recent (🔺 spiking). Where the data can be joined, each suite also shows its job pass rate.

### 📉 Job Success Rate

Per-job-type success percentage across autoland, mozilla-central and mozilla-beta, worst first — a port of the perf Redash dashboard's query onto the public REST API.

---

## Features

- **Dual time windows** — primary window (default 7d) and a 2-day snapshot for each bug, showing recent activity alongside the weekly view
- **Per-push failure rate** — failures per push to the tree (Treeherder `/failurecount/`)
- **Per-job success rate** — share of a job type's runs that passed (Treeherder jobs API); see the caveats below
- **Week-over-week trend** — `↑ +N` / `↓ N` comparing the current 7d window against the prior 7d window
- **Platform and repository breakdown** — for both 7d and 2d windows
- **Bug age**, **Assigned To**, and **NEEDINFO** tracking
- **OrangeFactor graph links** per bug
- Daily report published at 0900 UTC to GitHub Pages

---

## The two different "rates"

The report shows two things that both sound like a failure rate. They are not the same and are not comparable:

| | Source | Denominator | Where |
|---|---|---|---|
| **Failure rate** | `/api/failurecount/` | **pushes** to the tree | Intermittent Triage tab, per bug |
| **Success rate** | `/api/project/{repo}/jobs/` | **job runs** | Test Suites + Job Success Rate tabs |

The first answers "how often does this bug bite a push", the second "how often does this job come back green". Only the second is a true per-job rate.

### Caveats on the per-job success rate

- **Suites without a rate are unknown, not perfect.** The Test Suites tab keys on Treeherder's `test_suite`; job stats key on `job_type_name`. They are bridged heuristically, and the join is partial — roughly 2/3 of suites match on real data. A suite showing no rate badge has no matching job data; it does **not** mean 100% pass.
- **Job groups are an explicit list.** The jobs API silently ignores unknown filter parameters (`job_type_name__contains` is accepted but not applied), so perf jobs are selected by enumerating `job_group_symbol` values in `perfJobGroups` in `jobs.go`. New harness variants — the network-benchmark harness mints a group per bandwidth/latency condition — will be missed until added. Run `--discover-job-groups` to detect drift:

  ```bash
  go run . --discover-job-groups
  ```

- **Android is included by default.** The original dashboard query has `AND job_type.name NOT LIKE '%android%'`; this tool keeps android, since android mozperftest jobs are currently the worst offenders and the suite view needs them. Pass `--rates-exclude-android` to match the dashboard exactly — it affects only the Job Success Rate table, so suite rate badges keep working for android suites either way.
- **A failed sweep is dropped, not partially counted.** Truncating a sweep shrinks the denominator while the failures survive, which would *inflate* the failure rate rather than merely lose data. Any repo/job-group sweep that fails or hits the page cap is discarded whole, and the report shows a banner naming how many were dropped.
- Job types with fewer than 20 runs in the window, and those at 100% success, are omitted from the Job Success Rate tab.

---

## Usage

### Run locally

```bash
go run .
```

Generates `report.html` and opens it in your browser.

Note it is `go run .` and not `go run main.go` — the package spans `main.go`, `suites.go` and `jobs.go`.

### CLI flags

| Flag                    | Default | Description                                             |
|-------------------------|---------|---------------------------------------------------------|
| `--no-open`             | false   | Do not open the browser after report generates          |
| `--concurrency`         | 10      | Max concurrent Treeherder API calls                     |
| `--threshold`           | 20      | Minimum failure count to include a bug                  |
| `--days`                | 7       | Primary window size in days                             |
| `--top`                 | 25      | Number of top failing suites / job types to show        |
| `--no-suites`           | false   | Skip the Test Suites tab                                |
| `--no-rates`            | false   | Skip the per-job success rate sweep (the slowest phase) |
| `--rates-exclude-android` | false | Exclude android job types from the Job Success Rate tab |
| `--discover-job-groups` | false   | Report perf job groups missing from `perfJobGroups`, then exit |

`--concurrency` is a ceiling on total in-flight Treeherder requests across all phases, not per-phase.

The full run makes roughly 100 extra paginated requests for the rate sweep. Use `--no-rates` for a quick iteration.

---

## Development

Run tests:

```bash
go test ./...
go test -race ./...
```

Enable the pre-push hook that runs tests before each push:

```bash
git config core.hooksPath .githooks
```

---

## Build

```bash
go build -o perftest-report .
./perftest-report --no-open
```

---

## Output

Latest published report: https://92kns.github.io/perftest-triage-report/

---

## Credits

- Original Python script by [@florinbilt](https://github.com/florinbilt)
- Developed and maintained by [@kshampur](https://github.com/92kns)

---

## License

MIT License. See `LICENSE` file.
