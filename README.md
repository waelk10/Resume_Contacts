# Job Search Agent

An end-to-end agentic pipeline for job search: from discovering leads and harvesting recruiter contacts, to collecting application URLs and autonomously filling and submitting forms across every major ATS platform.

```
discover ──► start ──► pages ──► apply
   │            │          │         │
Find new    Harvest     Collect   Fill & submit
job board   recruiter   direct    forms with CV
sources     emails      ATS URLs  data + AI tailoring
```

Each stage feeds the next. Run them continuously in parallel or as a one-shot pipeline depending on your workflow.

---

## Build

```bash
make          # native binary → Resume_Contacts_Scraper
make linux    # → Resume_Contacts_Scraper.bin  (linux/amd64, static)
make windows  # → Resume_Contacts_Scraper.exe  (windows/amd64, static)
make all      # both cross-compile targets
```

Other make targets:

```bash
make tidy   # go mod tidy + verify
make test   # run tests with -race
make lint   # golangci-lint
make clean  # remove binaries
```

---

## Pipeline

### Stage 1 — `discover`: find new lead sources

BFS meta-source discovery that crawls known job aggregators and finds new job boards, then appends them to `discovered_seeds.txt`. Feed the results into `start` or `pages` to expand coverage over time.

```bash
./Resume_Contacts_Scraper discover [flags]
```

Output: `discovered_seeds.txt`

---

### Stage 2 — `start`: harvest recruiter contacts

Continuous spider that crawls job boards and Hacker News "Who is Hiring?" threads to collect recruiter and hiring-manager email addresses. Runs until `Ctrl+C`; re-seeds every 30 minutes and re-checks HN hourly.

```bash
./Resume_Contacts_Scraper start [flags]
```

Output: `contacts/<email>.vcf` — one vCard 3.0 file per unique address.

---

### Stage 3 — `pages`: collect application-page URLs

Finite crawl that extracts direct job-application URLs from ATS platforms (Greenhouse, Lever, Workday, iCIMS, Ashby, SmartRecruiters, Workable, BambooHR, Personio, Recruitee, Breezy, Jobvite, and more). Exits when the crawl completes.

```bash
./Resume_Contacts_Scraper pages [flags]
```

Output: `application_pages.txt` — one URL per line, ready to pass to `apply`.

---

### Stage 4 — `apply`: autonomous form automation

Reads the URL list and fills every form using a headless Firefox browser. Fields are populated from flags; anything not specified is auto-extracted from the CV PDF. An optional Claude integration tailors the resume to each job before uploading. Requires `geckodriver` in `PATH`.

```bash
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com [flags]
```

#### How it works

1. **CV extraction** — name, address, education, links, and other fields are parsed directly from the PDF so most flags are optional.
2. **Platform detection** — each URL is routed to a platform-specific filler (Greenhouse, Ashby, Lever, Workday, Personio, and others) before a generic fallback handles any remaining fields.
3. **AI resume tailoring** — when `--tailor` is set the `claude` CLI is called per job to rewrite the resume against the job description, and the tailored PDF is uploaded in place of the base CV.
4. **Simplify integration** — if a persistent Firefox profile with the Simplify extension is supplied, the extension auto-fills first and the agent fills anything it missed.
5. **Resilience** — CAPTCHA detection, email-verification cooldowns (65 min on Greenhouse), platform rate-limit back-off (90 s on Ashby), and automatic re-queuing keep the pipeline running unattended.

#### Required flags

| Flag | Description |
|---|---|
| `-u`, `--urls FILE` | Line-separated file of job-application URLs |
| `-r`, `--resume FILE` | Path to your CV/resume PDF |
| `--name NAME` | Your full name |
| `--email EMAIL` | Your email address |

#### Contact / address (auto-extracted from CV when omitted)

| Flag | Description |
|---|---|
| `--phone PHONE` | Phone number |
| `--linkedin URL` | LinkedIn profile URL |
| `--city`, `--state`, `--zip`, `--country` | Address fields |
| `--website URL` | Personal website / portfolio |
| `--github URL` | GitHub profile URL |

#### Education (auto-extracted from CV when omitted)

| Flag | Description |
|---|---|
| `--school NAME` | University or school name |
| `--degree LEVEL` | `bachelor` \| `master` \| `phd` \| `associate` |
| `--field-of-study TEXT` | Major or field of study |

When a school or field of study is not found in the ATS's curated list, the form falls back to selecting "Other".

#### Compensation / availability

| Flag | Default | Description |
|---|---|---|
| `--salary VALUE` | `Negotiable` | Expected salary answer (e.g. `"85000"`, `"80k-100k"`) |
| `--notice-period TEXT` | `2 weeks` | Answer to notice-period fields |
| `--start-date YYYY-MM-DD` | today + 14 days | Earliest available start date for date-picker inputs |

#### Work eligibility

| Flag | Default | Description |
|---|---|---|
| `--work-auth yes\|no` | `yes` | Answer to "authorized to work?" questions |
| `--sponsorship yes\|no` | `no` | Answer to "require visa sponsorship?" questions |

#### Voluntary self-identification (EEO)

| Flag | Default | Accepted values |
|---|---|---|
| `--gender` | `decline` | `male \| female \| non-binary \| decline` |
| `--ethnicity` | `decline` | `white \| black \| hispanic \| asian \| american-indian \| pacific-islander \| two-or-more \| decline` |

`decline` selects "Prefer not to answer" / "Decline to self-identify" where present.

#### Browser / behaviour

| Flag | Default | Description |
|---|---|---|
| `-c`, `--concurrency` | `1` | Parallel browser pages (keep ≤ 2 to avoid detection) |
| `--dry-run` | off | Fill forms but do not click Submit |
| `--headful` | off | Show the browser window |
| `--hold` | off | Keep each window open until you close it, then move to the next URL (implies `--headful`) |
| `--screenshots` | off | Save a PNG screenshot after filling each form |
| `--cover-letter FILE` | | Path to a plain-text cover letter injected into cover-letter fields |

#### Resume tailoring (Claude)

| Flag | Default | Description |
|---|---|---|
| `--tailor` | off | Call the `claude` CLI to generate a position-specific resume PDF before each upload |
| `--output-dir DIR` | `tailored_resumes/` | Directory for tailored resume PDFs |

#### Simplify extension

| Flag | Default | Description |
|---|---|---|
| `--profile-dir DIR` | platform default | Persistent Firefox profile with the Simplify extension installed and authenticated |
| `--simplify-wait N` | `0` | Seconds to pause after form load for Simplify to auto-fill (set to `3` when using `--profile-dir`) |
| `--setup` | off | Open Firefox with `--profile-dir` so you can install and log in to Simplify, then close the window |

#### Output / debugging

| Flag | Default | Description |
|---|---|---|
| `--failed-urls FILE` | `failed_urls.txt` | Append URLs that errored to this file for later retry (empty string disables) |
| `--log-file FILE` | `apply.log` | Write all log output to this file (appended across runs; empty string disables) |

The log file captures every `[apply]`, `[tailor]`, `[cv]`, and `[greenhouse]`/`[ashby]` diagnostic message with microsecond timestamps, the per-URL result, and a final summary. Output is also mirrored to stderr.

---

### Shared flags (`start`, `pages`, `discover`)

| Flag | Default | Description |
|---|---|---|
| `-c`, `--concurrency` | `4` | Concurrent requests per domain (max 128) |
| `-s`, `--seeds FILE` | | Extra seed URLs to add to the built-in list (line-separated file) |
| `--countries CODES` | all | Comma-separated country/region codes to restrict which seeds are visited |
| `--hops N` | `2` | BFS depth for `discover` (ignored by `start`/`pages`) |
| `--smtp-verify` | off | Probe each email's mail server before saving (`start` only; adds latency) |

#### Country codes

| Code | Meaning |
|---|---|
| `de at ch nl be lu fr es pt it gr mt gb ie` | Individual country ISO codes |
| `dk se no fi is pl cz hu ro bg hr si sk` | Individual country ISO codes (cont.) |
| `il us ca au` | Israel, US, Canada, Australia |
| `global` | Boards with no geographic focus |
| `eu` | Pan-European boards |
| `dach` | Alias → `de, at, ch` |
| `benelux` | Alias → `nl, be, lu` |
| `nordics` | Alias → `dk, se, no, fi, is` |
| `cee` | Alias → `pl, cz, hu, ro, bg, hr, si, sk` |
| `southern` | Alias → `es, pt, it, gr, mt` |

---

### `clean` — filter and deduplicate contacts

Cleans `contacts/*.vcf` in-place.

```bash
./Resume_Contacts_Scraper clean [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-d`, `--dir DIR` | `contacts` | Contacts directory to clean |
| `-f`, `--filter-regex RE` | | Remove contacts whose email matches this Go regexp |
| `--dedup` | off | Deduplicate by email, keeping the card with the most information |

At least one of `--filter-regex` or `--dedup` is required.

---

### `purge` — wipe all output

Deletes all persistent output from previous runs: `contacts/`, `application_pages.txt`, `discovered_seeds.txt`. Asks for confirmation unless `--yes` is passed.

```bash
./Resume_Contacts_Scraper purge [--yes]
```

---

## Examples

```bash
# ── Stage 1: discover new seed sources ───────────────────────────────────────
./Resume_Contacts_Scraper discover --hops 3 --countries dach,global
./Resume_Contacts_Scraper discover --hops 6 --countries de,il,us,ca,uk,pt,cz,at

# ── Stage 2: harvest recruiter contacts ──────────────────────────────────────
./Resume_Contacts_Scraper start -c 8
./Resume_Contacts_Scraper start -c 16 --countries de,dach,eu,global -s discovered_seeds.txt
./Resume_Contacts_Scraper start --countries gb,ie,global --smtp-verify

# ── Stage 3: collect application-page URLs ────────────────────────────────────
./Resume_Contacts_Scraper pages -c 16 --countries de,ie,pt,cz,il,at -s discovered_seeds.txt
./Resume_Contacts_Scraper pages --countries dach

# ── Stage 4: apply ───────────────────────────────────────────────────────────

# Dry run — fill forms, watch in browser, don't submit
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com --dry-run --headful

# Full run with all details and logging
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com \
  --phone "+1 555 0100" --linkedin "https://linkedin.com/in/janedoe" \
  --log-file apply.log

# Claude-tailored resumes + Simplify autofill
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com \
  --tailor --profile-dir ~/.mozilla/resume-scraper-profile --simplify-wait 3

# Resume a partially-completed batch (URLs not attempted in a previous run)
./Resume_Contacts_Scraper apply --urls remaining_urls.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com

# ── Maintenance ───────────────────────────────────────────────────────────────

# Remove noreply addresses and deduplicate contacts
./Resume_Contacts_Scraper clean --filter-regex '^(noreply|no-reply|donotreply)@' --dedup

# Wipe all output and start fresh
./Resume_Contacts_Scraper purge --yes
```

---

## Notes

- `start` and `pages` resume safely if interrupted — existing output is read on startup and duplicates are skipped.
- Domains with 3 consecutive failures are skipped for the remainder of the run.
- `apply` enforces per-platform cooldowns (e.g. 90 s between Ashby submissions, 65 min after a Greenhouse email-verification trigger) and re-queues affected URLs automatically so other jobs keep processing in the meantime.
- Education fields (school, degree, field of study) are auto-extracted from the CV PDF. When an ATS's school or field-of-study list does not contain the parsed value, the form selects "Other" rather than picking an arbitrary entry.
- Request timeout: 30 s. Max response body: 2 MB.
