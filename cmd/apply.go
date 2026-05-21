package cmd

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"Resume_Contacts_Scraper/internal/applier"
)

func applyJobs() {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: Resume_Contacts_Scraper apply [flags]

Reads a list of job-application URLs and fills each form using a headless
browser, uploading the supplied CV PDF.

Required flags:
  --urls FILE      file listing job URLs, one per line
  --resume FILE    path to your CV/resume PDF
  --name NAME      your full name
  --email EMAIL    your email address

Address / contact (auto-extracted from CV when omitted):
  --city, --state, --zip, --country, --website, --github

Compensation / availability (auto-filled; override with these flags):
  --salary VALUE              expected salary answer, e.g. "85000" or "80k-100k"
                              (default: "Negotiable"; overrides the baked-in default)
  --notice-period TEXT        notice-period answer, e.g. "immediately", "1 month"
                              (default: "2 weeks")
  --start-date YYYY-MM-DD     earliest start date for date-picker inputs
                              (default: today + 14 days)

Work eligibility:
  --work-auth yes|no          answer to "authorized to work?" (default: yes)
  --sponsorship yes|no        answer to "require sponsorship?" (default: no)

Voluntary self-identification (EEO) — Ashby and other ATS platforms:
  --gender VALUE
      male | female | non-binary | decline (default: decline)
      "decline" selects "Prefer not to answer" / "Decline to self-identify".

  --ethnicity VALUE
      white | black | hispanic | asian | american-indian |
      pacific-islander | two-or-more | decline (default: decline)
      "decline" selects "Prefer not to answer" / "Decline to self-identify".

All flags:
`)
		fs.PrintDefaults()
	}

	var (
		urlsFile      string
		resumePath    string
		name          string
		email         string
		phone         string
		linkedin      string
		dryRun        bool
		hold          bool
		conc          int
		headful       bool
		shots         bool
		tailor        bool
		outputDir     string
		failedURLsFile string
		logFile        string
		// Extended applicant fields
		city               string
		state              string
		zipCode            string
		country            string
		school             string
		degree             string
		fieldOfStudy       string
		website            string
		github             string
		coverLetterPath    string
		expectedSalary     string
		noticePeriod       string
		startDate          string
		workAuth           string
		requireSponsorship string
		gender             string
		ethnicity          string
		// Simplify extension
		profileDir   string
		simplifyWait int
		setup        bool
	)

	fs.StringVar(&urlsFile, "urls", "", "file with job URLs, one per line [required]")
	fs.StringVar(&urlsFile, "u", "", "shorthand for --urls")
	fs.StringVar(&resumePath, "resume", "", "path to your CV PDF [required]")
	fs.StringVar(&resumePath, "r", "", "shorthand for --resume")
	fs.StringVar(&name, "name", "", "your full name [required]")
	fs.StringVar(&email, "email", "", "your email address [required]")
	fs.StringVar(&phone, "phone", "", "your phone number")
	fs.StringVar(&linkedin, "linkedin", "", "LinkedIn profile URL")
	fs.BoolVar(&dryRun, "dry-run", false, "fill forms but do not click Submit")
	fs.BoolVar(&hold, "hold", false, "keep each window open until you close it, then move to the next URL (implies --headful)")
	fs.IntVar(&conc, "concurrency", 1, "parallel browser pages (keep low to avoid detection)")
	fs.IntVar(&conc, "c", 1, "shorthand for --concurrency")
	fs.BoolVar(&headful, "headful", false, "show the browser window (useful for debugging)")
	fs.BoolVar(&shots, "screenshots", false, "save a PNG screenshot after filling each form")
	fs.BoolVar(&tailor, "tailor", false, "use the claude CLI to tailor the resume to each job before uploading")
	fs.StringVar(&outputDir, "output-dir", "tailored_resumes", "directory for tailored resume PDFs")
	fs.StringVar(&failedURLsFile, "failed-urls", "failed_urls.txt", "file to append URLs that failed to apply (empty string disables)")
	fs.StringVar(&logFile, "log-file", "apply.log", "file to write all log output (appended; empty string disables)")
	// Extended applicant details — auto-populated from the CV when omitted
	fs.StringVar(&city, "city", "", "city (auto-extracted from CV when omitted)")
	fs.StringVar(&state, "state", "", "state/province code or full name (auto-extracted from CV)")
	fs.StringVar(&zipCode, "zip", "", "ZIP / postal code (auto-extracted from CV)")
	fs.StringVar(&country, "country", "", "country (auto-extracted from CV)")
	fs.StringVar(&school, "school", "", "university/school name for education fields (auto-extracted from CV)")
	fs.StringVar(&degree, "degree", "", `degree level for education fields: bachelor | master | phd | associate (auto-extracted from CV)`)
	fs.StringVar(&fieldOfStudy, "field-of-study", "", "field of study / major for education fields (auto-extracted from CV)")
	fs.StringVar(&website, "website", "", "personal website / portfolio URL (auto-extracted from CV)")
	fs.StringVar(&github, "github", "", "GitHub profile URL (auto-extracted from CV)")
	fs.StringVar(&coverLetterPath, "cover-letter", "", "path to a plain-text cover letter file")
	fs.StringVar(&expectedSalary, "salary", "", `override expected salary answer (e.g. "85000", "80k-100k"); default: "Negotiable"`)
	fs.StringVar(&noticePeriod, "notice-period", "2 weeks", `answer to "notice period" selects and text fields (e.g. "2 weeks", "immediately", "1 month")`)
	fs.StringVar(&startDate, "start-date", time.Now().AddDate(0, 0, 14).Format("2006-01-02"),
		"earliest start date (YYYY-MM-DD) for date-picker inputs; default: today + 14 days")
	fs.StringVar(&workAuth, "work-auth", "yes", `"yes"/"no" answer to work-authorization questions`)
	fs.StringVar(&requireSponsorship, "sponsorship", "no", `"yes"/"no" answer to visa-sponsorship questions`)
	fs.StringVar(&gender, "gender", "decline",
		`voluntary self-ID gender answer (Ashby and other EEO radio sections).
	Accepted values: male | female | non-binary | decline
	"decline" selects "Prefer not to answer" / "Decline to self-identify".`)
	fs.StringVar(&ethnicity, "ethnicity", "decline",
		`voluntary self-ID ethnicity/race answer (Ashby and other EEO radio sections).
	Accepted values:
	  white | black | hispanic | asian |
	  american-indian | pacific-islander | two-or-more | decline
	"decline" selects "Prefer not to answer" / "Decline to self-identify".`)
	// Simplify extension
	fs.StringVar(&profileDir, "profile-dir", defaultProfileDir(),
		"persistent Firefox profile directory that holds the Simplify extension\n"+
			"\t(created automatically; run with --setup once to install and log in)")
	fs.IntVar(&simplifyWait, "simplify-wait", 0,
		"seconds to pause after form load for the Simplify extension to auto-fill\n"+
			"\t(set to 3 when using --profile-dir; 0 disables the pause)")
	fs.BoolVar(&setup, "setup", false,
		"open Firefox with --profile-dir so you can install and log into Simplify,\n"+
			"\tthen close the window — no other flags required")

	_ = fs.Parse(os.Args[2:])

	// --setup: interactive one-time Simplify login — no other flags required.
	if setup {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := applier.RunSetup(ctx, profileDir); err != nil {
			log.Fatalf("setup: %v", err)
		}
		return
	}

	var missing []string
	if urlsFile == "" {
		missing = append(missing, "--urls")
	}
	if resumePath == "" {
		missing = append(missing, "--resume")
	}
	if name == "" {
		missing = append(missing, "--name")
	}
	if email == "" {
		missing = append(missing, "--email")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "error: missing required flags: %s\n\n", strings.Join(missing, ", "))
		fs.Usage()
		os.Exit(1)
	}

	if _, err := os.Stat(resumePath); err != nil {
		fmt.Fprintf(os.Stderr, "error: resume file not found: %s\n", resumePath)
		os.Exit(1)
	}

	// Set up log file — tee all log output to both stderr and the file.
	var logWriter io.Writer = os.Stderr
	if logFile != "" {
		lf, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not open log file %q: %v\n", logFile, err)
		} else {
			defer lf.Close()
			logWriter = io.MultiWriter(os.Stderr, lf)
			fmt.Fprintf(lf, "\n=== apply run started %s ===\n", time.Now().Format(time.RFC3339))
		}
	}
	log.SetOutput(logWriter)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	urls, err := readLines(urlsFile)
	if err != nil {
		log.Fatalf("load URLs: %v", err)
	}
	if len(urls) == 0 {
		fmt.Println("No URLs found in the file — nothing to do.")
		return
	}

	// Load cover letter text from file if supplied.
	var coverLetterText string
	if coverLetterPath != "" {
		if data, err := os.ReadFile(coverLetterPath); err == nil {
			coverLetterText = strings.TrimSpace(string(data))
		} else {
			log.Printf("warning: could not read cover-letter file %q: %v", coverLetterPath, err)
		}
	}

	// Build a set of remaining URLs so we can remove each one from the input
	// file as it completes, giving a live "work remaining" view at a glance.
	remaining := make(map[string]bool, len(urls))
	for _, u := range urls {
		remaining[u] = true
	}
	var remainingMu sync.Mutex
	removeFromURLsFile := func(r applier.Result) {
		if r.Status == "pending" {
			return // never attempted — leave in the file
		}
		remainingMu.Lock()
		defer remainingMu.Unlock()
		delete(remaining, r.URL)
		if err := rewriteURLsFile(urlsFile, urls, remaining); err != nil {
			log.Printf("warning: could not update %s: %v", urlsFile, err)
		}
	}

	cfg := applier.Config{
		Applicant: applier.ApplicantInfo{
			Name:               name,
			Email:              email,
			Phone:              phone,
			LinkedInURL:        linkedin,
			City:               city,
			State:              state,
			ZipCode:            zipCode,
			Country:            country,
			School:             school,
			Degree:             degree,
			FieldOfStudy:       fieldOfStudy,
			Website:            website,
			GitHubURL:          github,
			CoverLetter:        coverLetterText,
			ExpectedSalary:     expectedSalary,
			NoticePeriod:       noticePeriod,
			EarliestStartDate:  startDate,
			WorkAuthorized:     workAuth,
			RequireSponsorship: requireSponsorship,
			Gender:             gender,
			Ethnicity:          ethnicity,
		},
		ResumePath:        resumePath,
		DryRun:            dryRun,
		Hold:              hold,
		Concurrency:       conc,
		Headful:           headful,
		Screenshots:       shots,
		TailorResumes:     tailor,
		TailoredOutputDir: outputDir,
		ProfileDir:        profileDir,
		SimplifyWait:      time.Duration(simplifyWait) * time.Second,
		OnResult:          removeFromURLsFile,
	}

	mode := "live"
	if dryRun {
		mode = "dry-run"
	}
	tailorNote := ""
	if tailor {
		tailorNote = "  |  Claude tailoring: ON"
	}
	fmt.Printf("Auto-Apply  |  URLs: %d  |  Mode: %s%s\n\n", len(urls), mode, tailorNote)

	app, err := applier.New(cfg)
	if err != nil {
		log.Fatalf("init applier: %v", err)
	}

	// closeOnce ensures app.Close() (kills geckodriver + all Firefox sessions)
	// is called exactly once whether we finish normally or get interrupted.
	var closeOnce sync.Once
	doClose := func() { closeOnce.Do(app.Close) }
	defer doClose()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// When Ctrl+C is pressed, cancel the context AND immediately kill geckodriver.
	// Killing the server is what actually unblocks in-flight Selenium calls;
	// context cancellation alone only stops new work from starting.
	go func() {
		<-ctx.Done()
		fmt.Fprintln(os.Stderr, "\nInterrupted — stopping browser...")
		doClose()
	}()

	results := app.Run(ctx, urls)

	var applied, dryCnt, skippedCnt, errCnt, pendingCnt int
	for _, r := range results {
		switch r.Status {
		case "applied":
			applied++
			line := fmt.Sprintf("[+] %s @ %s", r.Title, r.Company)
			fmt.Println(line)
			log.Print(line)
		case "dry-run":
			dryCnt++
			line := fmt.Sprintf("[~] %s @ %s  (dry-run)", r.Title, r.Company)
			fmt.Println(line)
			log.Print(line)
		case "skipped":
			skippedCnt++
			line := fmt.Sprintf("[-] %s  (window closed)", r.URL)
			fmt.Println(line)
			log.Print(line)
		case "pending":
			// URL was never attempted (context cancelled before the worker
			// reached it).  Not logged individually — captured in the summary
			// and saved to remaining_urls.txt for the next run.
			pendingCnt++
		default:
			errCnt++
			line := fmt.Sprintf("[!] %s: %v", r.URL, r.Error)
			fmt.Println(line)
			log.Print(line)
		}
	}
	summary := fmt.Sprintf("Done. Applied: %d  Dry-run: %d  Skipped: %d  Errors: %d  Pending: %d",
		applied, dryCnt, skippedCnt, errCnt, pendingCnt)
	fmt.Println()
	fmt.Println(summary)
	log.Print(summary)

	if errCnt > 0 && failedURLsFile != "" {
		if err := appendFailedURLs(failedURLsFile, results); err != nil {
			log.Printf("warning: could not save failed URLs: %v", err)
		} else {
			fmt.Printf("Failed URLs appended to: %s\n", failedURLsFile)
		}
	}

	// Save URLs that were never attempted (context cancelled) to a separate
	// file so the next run can resume without re-processing already-tried URLs.
	if pendingCnt > 0 {
		const remainingFile = "remaining_urls.txt"
		if err := appendRemainingURLs(remainingFile, results); err != nil {
			log.Printf("warning: could not save remaining URLs: %v", err)
		} else {
			fmt.Printf("Remaining URLs (not attempted): %d → %s  (use as --urls to resume)\n",
				pendingCnt, remainingFile)
			log.Printf("remaining URLs: %d → %s", pendingCnt, remainingFile)
		}
	}
}

// appendFailedURLs appends the URL of every error-status result to path, one
// per line.  "pending" results (never attempted) are excluded — they go to
// remaining_urls.txt instead.  The file is created if it does not exist;
// existing content is preserved so successive runs accumulate a retry list.
func appendFailedURLs(path string, results []applier.Result) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range results {
		if r.Status == "error" {
			fmt.Fprintln(w, r.URL)
		}
	}
	return w.Flush()
}

// appendRemainingURLs appends URLs that were never attempted (status "pending")
// to path so the next run can resume without re-processing already-tried URLs.
func appendRemainingURLs(path string, results []applier.Result) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range results {
		if r.Status == "pending" {
			fmt.Fprintln(w, r.URL)
		}
	}
	return w.Flush()
}

// defaultProfileDir returns a Firefox profile path that is accessible inside
// the snap sandbox on Ubuntu 22.04+ (where Firefox ships as a snap).
// Snap Firefox silently ignores profile paths outside its sandbox and falls
// back to the default profile, causing "Firefox is already running" errors.
// ~/snap/firefox/common/ is always writable by the snap; for non-snap Firefox
// it is just an ordinary directory that Firefox can access normally.
func defaultProfileDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "firefox-profile")
	}
	// If snap Firefox is installed, use its writable home directory.
	snapBase := filepath.Join(home, "snap", "firefox")
	if _, err := os.Stat(snapBase); err == nil {
		return filepath.Join(snapBase, "common", "resume-scraper-profile")
	}
	return filepath.Join(home, ".mozilla", "resume-scraper-profile")
}

// rewriteURLsFile rewrites path to contain only the URLs in allURLs whose key
// is still present in remaining.  Original ordering is preserved.  Writes are
// performed atomically via a temp file + rename so a concurrent read always
// sees a complete snapshot.
func rewriteURLsFile(path string, allURLs []string, remaining map[string]bool) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, u := range allURLs {
		if remaining[u] {
			fmt.Fprintln(w, u)
		}
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

// readLines reads a file and returns non-blank, non-comment lines.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines, sc.Err()
}
