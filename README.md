# Resume Contacts Scraper

Crawls job boards and ATS platforms to collect recruiter/hiring-manager contact emails and submit job applications automatically.

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

## Commands

```
Resume_Contacts_Scraper <command> [flags]
```

---

### `start` — scrape contact emails

Crawls job boards and HN "Who is Hiring?" threads for recruiter/hiring-manager emails. Runs continuously until `Ctrl+C`; the web spider re-seeds every 30 minutes and HN is re-checked hourly.

```bash
./Resume_Contacts_Scraper start [flags]
```

Output: `contacts/<email>.vcf` (one vCard 3.0 file per unique address)

---

### `pages` — collect application-page URLs

Finite crawl that collects direct job-application URLs from ATS platforms (Greenhouse, Lever, Workday, iCIMS, Ashby, and more). Exits when the crawl completes.

```bash
./Resume_Contacts_Scraper pages [flags]
```

Output: `application_pages.txt` (one URL per line)

---

### `discover` — find new seed sources

BFS meta-source discovery that finds new job boards and appends them to `discovered_seeds.txt`. Feed the results back into `start` or `pages` with `-s discovered_seeds.txt`.

```bash
./Resume_Contacts_Scraper discover [flags]
```

Output: `discovered_seeds.txt`

---

### Shared flags (`start`, `pages`, `discover`)

| Flag | Default | Description |
|---|---|---|
| `-c`, `--concurrency` | `4` | Concurrent requests per domain (max 128) |
| `-s`, `--seeds FILE` | | Extra seed URLs to add to the built-in list (line-separated file) |
| `--countries CODES` | all | Comma-separated country/region codes to restrict which seeds are visited |
| `--hops N` | `2` | BFS depth for `discover` — extra meta-source hops beyond the initial list (ignored by `start`/`pages`) |
| `--smtp-verify` | off | Probe each email's mail server to confirm the address exists before saving (`start` only; adds latency) |

#### Country codes

| Code | Meaning |
|---|---|
| `de at ch nl be lu fr es pt it gr mt gb ie` | Individual country ISO codes |
| `dk se no fi is pl cz hu ro bg hr si sk` | Individual country ISO codes (cont.) |
| `global` | Boards with no geographic focus |
| `eu` | Pan-European boards |
| `dach` | Alias → `de, at, ch` |
| `benelux` | Alias → `nl, be, lu` |
| `nordics` | Alias → `dk, se, no, fi, is` |
| `cee` | Alias → `pl, cz, hu, ro, bg, hr, si, sk` |
| `southern` | Alias → `es, pt, it, gr, mt` |

---

### `apply` — headless auto-apply

Reads a list of job-application URLs and fills each form using a headless Firefox browser, uploading your CV PDF. Requires `geckodriver` in `PATH`.

```bash
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com [flags]
```

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

#### Resume tailoring

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
| `--log-file FILE` | `apply.log` | Write all log output to this file (appended across runs; empty string disables). Each run is marked with a timestamped header. Useful for debugging intermittent form-fill failures. |

The log file captures every `[apply]`, `[tailor]`, and `[cv]` diagnostic message with microsecond timestamps, plus the per-URL result and final summary. Log output is also mirrored to stderr.

---

### `clean` — filter and deduplicate contacts

Cleans up `contacts/*.vcf` files in-place.

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

Deletes all persistent output from previous runs: `contacts/contacts_*.vcf`, `application_pages.txt`, `discovered_seeds.txt`. Asks for confirmation unless `--yes` is passed.

```bash
./Resume_Contacts_Scraper purge [--yes]
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip confirmation prompt and delete immediately |

---

## Examples

```bash
# Scrape contacts indefinitely (Ctrl+C to stop)
./Resume_Contacts_Scraper start
./Resume_Contacts_Scraper start -c 8
./Resume_Contacts_Scraper start --countries de,dach,eu,global
./Resume_Contacts_Scraper start --countries gb,ie,global -s my_seeds.txt

# Collect application-page URLs
./Resume_Contacts_Scraper pages --concurrency 6 --seeds extra.txt
./Resume_Contacts_Scraper pages --countries dach

# Discover new seed sources then feed them back in
./Resume_Contacts_Scraper discover --hops 3 --countries dach,global
./Resume_Contacts_Scraper start -s discovered_seeds.txt

# Dry-run apply (fill forms, don't submit; watch in the browser)
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com --dry-run --headful

# Live apply with full details and logging
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com \
  --phone "+1 555 0100" --linkedin "https://linkedin.com/in/janedoe" \
  --log-file apply.log

# Apply with Claude-tailored resumes and Simplify auto-fill
./Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
  --name "Jane Doe" --email jane@example.com \
  --tailor --profile-dir ~/.mozilla/resume-scraper-profile --simplify-wait 3

# Clean contacts: remove noreply addresses and deduplicate
./Resume_Contacts_Scraper clean --filter-regex '^(noreply|no-reply|donotreply)@' --dedup

# Wipe all output
./Resume_Contacts_Scraper purge --yes
```

## Notes

- Both `start` and `pages` resume safely if interrupted — existing output is read on startup and duplicates are skipped.
- Domains that fail 3 consecutive requests are skipped for the remainder of the run.
- The `apply` command enforces per-platform cooldowns (e.g. 90 s between Ashby submissions) to avoid spam detection.
- Request timeout: 30 s. Max response body: 2 MB.
