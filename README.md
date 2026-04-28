# Resume Contacts Scraper

Crawls job boards and ATS platforms to collect recruiter/hiring-manager contact emails and job application-page URLs.

## Build

```bash
make          # native binary → Resume_Contacts_Scraper
make linux    # → Resume_Contacts_Scraper.bin  (linux/amd64, static)
make windows  # → Resume_Contacts_Scraper.exe  (windows/amd64, static)
make all      # both cross-compile targets
```

## Commands

### `start` — scrape contact emails

Crawls job boards and HN "Who is Hiring?" threads for recruiter emails.

```bash
./Resume_Contacts_Scraper start [-c N]
```

Output: `contacts/contacts_001.vcf`, `contacts_002.vcf`, … (100 contacts per file, vCard 3.0)

### `pages` — scrape application-page URLs

Crawls job boards and ATS platforms (Greenhouse, Lever, Workday, iCIMS, Ashby, and more) to collect direct job application URLs.

```bash
./Resume_Contacts_Scraper pages [-c N]
```

Output: `application_pages.txt` (one URL per line)

### Flags

| Flag | Default | Description |
|---|---|---|
| `-c`, `--concurrency` | `4` | Concurrent requests per domain (max 8) |

## Other targets

```bash
make tidy   # go mod tidy + verify
make test   # run tests with -race
make lint   # golangci-lint
make clean  # remove binaries
```

## Notes

- Both commands resume safely if interrupted — existing output is read on startup and duplicates are skipped.
- Domains that fail 3 consecutive requests are skipped for the remainder of the run.
- Request timeout: 30 s. Max response body: 2 MB.
