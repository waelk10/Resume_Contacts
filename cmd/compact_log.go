package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"Resume_Contacts_Scraper/internal/applylog"
)

const defaultCompactLogFile = "apply_compact.jsonl"

var (
	reBanner  = regexp.MustCompile(`^=== apply run started (.+) ===$`)
	reLogLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d+) (.+)$`)
	reApply   = regexp.MustCompile(`^\[apply\] "([^"]+)" @ (.+?)\s{2,}platform=(\S+)$`)
	reApplied = regexp.MustCompile(`^\[\+\] (.+?) @ (.+)$`)
	reDryRun  = regexp.MustCompile(`^\[~\] (.+?) @ (.+?)\s+\(dry-run\)$`)
	reSkipped = regexp.MustCompile(`^\[-\] (\S+)\s+\(window closed\)$`)
	reErrLine = regexp.MustCompile(`^\[!\] (https?://\S+): (.+)$`)
	reSummary = regexp.MustCompile(`^Done\. Applied:`)
)

func compactLog() {
	fs := flag.NewFlagSet("compact-log", flag.ExitOnError)
	inFile := fs.String("log", "apply.log", "verbose apply log to parse")
	outFile := fs.String("out", defaultCompactLogFile, "output compact JSONL file")
	truncate := fs.Bool("truncate-log", false, "replace the verbose log with session summaries after compacting (reduces size dramatically)")
	_ = fs.Parse(os.Args[2:])

	fmt.Printf("Parsing %s ...\n", *inFile)
	parsed, keepers, err := parseVerboseLog(*inFile)
	if err != nil {
		log.Fatalf("parse: %v", err)
	}

	// Merge with any existing compact records.
	existing, err := applylog.ReadAll(*outFile)
	if err != nil {
		log.Fatalf("read compact log: %v", err)
	}
	merged := applylog.DeduplicateByURL(append(existing, parsed...))

	// Write atomically.
	tmp := *outFile + ".tmp"
	tf, err := os.Create(tmp)
	if err != nil {
		log.Fatalf("create temp file: %v", err)
	}
	w := applylog.NewWriterToFile(tf)
	for _, r := range merged {
		if err := w.Write(r); err != nil {
			tf.Close()
			os.Remove(tmp)
			log.Fatalf("write: %v", err)
		}
	}
	tf.Close()
	if err := os.Rename(tmp, *outFile); err != nil {
		log.Fatalf("rename: %v", err)
	}

	// Report.
	var nApplied, nDry, nSkipped, nErr, nNoURL int
	for _, r := range merged {
		switch r.Status {
		case "applied":
			nApplied++
		case "dry-run":
			nDry++
		case "skipped":
			nSkipped++
		case "error":
			nErr++
		}
		if r.URL == "" {
			nNoURL++
		}
	}
	fmt.Printf("Compact log: %d records  (applied: %d  dry-run: %d  skipped: %d  errors: %d)\n",
		len(merged), nApplied, nDry, nSkipped, nErr)
	if nNoURL > 0 {
		fmt.Printf("Note: %d records have no URL (from old verbose-log format where outcome lines omit the URL)\n", nNoURL)
	}
	fmt.Printf("Written to: %s\n\n", *outFile)

	if *truncate {
		if err := truncateVerboseLog(*inFile, keepers); err != nil {
			log.Printf("warning: could not truncate verbose log: %v", err)
		} else {
			fmt.Printf("Verbose log truncated to session summaries: %s\n", *inFile)
		}
	} else {
		fmt.Printf("Tip: run with --truncate-log to replace %s with session summaries only\n", *inFile)
	}
}

// parseVerboseLog reads the verbose apply.log and returns compact Records.
// It also returns "keeper" lines (banners + summaries) for --truncate-log.
// The parser is concurrent-safe: it tracks in-flight jobs by title+company key
// rather than a single current-job variable.
func parseVerboseLog(path string) ([]applylog.Record, []string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var records []applylog.Record
	var keepers []string

	runID := ""
	// inflight maps "title@company" → platform, tracking concurrent [apply] lines.
	inflight := make(map[string]string)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	for sc.Scan() {
		raw := sc.Text()

		// Session banner (written directly — no log timestamp prefix).
		if m := reBanner.FindStringSubmatch(raw); m != nil {
			runID = m[1]
			keepers = append(keepers, "", raw)
			continue
		}

		// All other lines carry a Go log timestamp.
		m := reLogLine.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		tsStr, msg := m[1], m[2]
		ts := parseLogTS(tsStr)

		// Summary line — keep for truncation.
		if reSummary.MatchString(msg) {
			keepers = append(keepers, raw)
			continue
		}

		// [apply] "Title" @ Company  platform=X — track job context.
		if am := reApply.FindStringSubmatch(msg); am != nil {
			key := am[1] + "\x00" + am[2]
			inflight[key] = am[3]
			continue
		}

		// [+] Title @ Company
		if am := reApplied.FindStringSubmatch(msg); am != nil {
			title, company := am[1], am[2]
			key := title + "\x00" + company
			platform := inflight[key]
			delete(inflight, key)
			records = append(records, applylog.Record{
				RunID: runID, TS: ts, Status: "applied",
				Title: title, Company: company, Platform: platform,
			})
			continue
		}

		// [~] Title @ Company  (dry-run)
		if am := reDryRun.FindStringSubmatch(msg); am != nil {
			title, company := am[1], am[2]
			key := title + "\x00" + company
			platform := inflight[key]
			delete(inflight, key)
			records = append(records, applylog.Record{
				RunID: runID, TS: ts, Status: "dry-run",
				Title: title, Company: company, Platform: platform,
			})
			continue
		}

		// [-] URL  (window closed)
		if am := reSkipped.FindStringSubmatch(msg); am != nil {
			u := am[1]
			records = append(records, applylog.Record{
				RunID: runID, TS: ts, Status: "skipped",
				URL: u, Platform: platformFromURL(u),
			})
			continue
		}

		// [!] URL: error
		if am := reErrLine.FindStringSubmatch(msg); am != nil {
			u := am[1]
			records = append(records, applylog.Record{
				RunID: runID, TS: ts, Status: "error",
				URL: u, Platform: platformFromURL(u), Error: am[2],
			})
			continue
		}
	}

	return records, keepers, sc.Err()
}

func parseLogTS(s string) time.Time {
	t, err := time.Parse("2006/01/02 15:04:05.000000", s)
	if err != nil {
		t, _ = time.Parse("2006/01/02 15:04:05", s)
	}
	return t
}

// platformFromURL returns a short platform name derived from the URL hostname.
func platformFromURL(u string) string {
	for _, p := range []struct{ sub, name string }{
		{"greenhouse.io", "greenhouse"},
		{"lever.co", "lever"},
		{"workday.com", "workday"},
		{"icims.com", "icims"},
		{"ashby.io", "ashby"},
		{"ashbyhq.com", "ashby"},
		{"smartrecruiters.com", "smartrecruiters"},
		{"jobvite.com", "jobvite"},
		{"taleo.net", "taleo"},
		{"breezy.hr", "breezy"},
		{"workable.com", "workable"},
		{"bamboohr.com", "bamboohr"},
		{"recruitee.com", "recruitee"},
	} {
		if strings.Contains(u, p.sub) {
			return p.name
		}
	}
	return "generic"
}

// truncateVerboseLog replaces path with just the keeper lines (banners + summaries).
func truncateVerboseLog(path string, keepers []string) error {
	tmp := path + ".compact.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, line := range keepers {
		fmt.Fprintln(w, line)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
