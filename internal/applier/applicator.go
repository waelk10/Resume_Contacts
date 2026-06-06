package applier

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"Resume_Contacts_Scraper/internal/resume"
)


// platformCooldowns is the minimum pause between consecutive applications on
// the same ATS platform.  Ashby's spam filters are sensitive to rapid
// submissions — a per-platform cooldown prevents the account from being flagged
// and applications silently dropped.
var platformCooldowns = map[string]time.Duration{
	"ashby":      60 * time.Second,
	"greenhouse": 20 * time.Second, // baseline; email-verify forces 65 min
	"workable":   30 * time.Second, // baseline; captcha forces 10 min
}

// ApplicantInfo holds the personal details typed into application forms.
type ApplicantInfo struct {
	// Core contact — always required
	Name        string
	Email       string
	Phone       string
	LinkedInURL string

	// Address — auto-extracted from CV when empty
	City    string
	State   string // 2-letter code or full name accepted
	ZipCode string
	Country string

	// Professional links — auto-extracted from CV when empty
	Website   string // personal site / portfolio
	GitHubURL string

	// Education — auto-extracted from CV when empty.
	// Degree is a normalised keyword: "bachelor", "master", "phd", "associate".
	School       string
	Degree       string
	FieldOfStudy string

	// Application text
	CoverLetter string // plain text; injected into cover-letter textareas

	// Expected salary — fills "expected salary" / "desired compensation" fields.
	// Examples: "85000", "$85,000", "80k-100k".  Empty means skip.
	ExpectedSalary string

	// NoticePeriod is the text answer to "notice period" selects and text
	// inputs (e.g. "2 weeks", "immediately", "1 month").
	// Default when empty: "2 weeks" (set by New()).
	NoticePeriod string

	// EarliestStartDate is an ISO-8601 date (YYYY-MM-DD) injected into
	// date-picker inputs for "available from" / "start date" questions.
	// Default when empty: today + 14 days (set by New()).
	EarliestStartDate string

	// Work eligibility — used to answer yes/no radio/select questions
	// Values: "yes" or "no".  Empty means "don't answer".
	WorkAuthorized     string // "Are you authorised to work…?"
	RequireSponsorship string // "Do you require sponsorship?"

	// Voluntary self-identification (EEO).  Both default to "decline" which
	// selects "Prefer not to answer" / "Decline to self-identify" when present.
	//
	// Accepted Gender values:
	//   male | female | non-binary | decline
	//
	// Accepted Ethnicity values:
	//   white | black | hispanic | asian |
	//   american-indian | pacific-islander | two-or-more | decline
	Gender    string
	Ethnicity string
}

// Config controls the auto-apply pipeline.
type Config struct {
	Applicant   ApplicantInfo
	ResumePath  string
	DryRun      bool // fill forms but do not click Submit
	Concurrency int  // max parallel browser pages (keep ≤ 2 to avoid detection)
	Headful     bool // show the browser window
	Screenshots bool // save a PNG after each form fill
	Hold        bool // keep each window open until the user closes it

	// OnResult, if non-nil, is called once for every URL that reaches a terminal
	// status (anything except "pending" — URLs that were never attempted because
	// the context was cancelled).  Called from worker goroutines; must be safe
	// for concurrent use.
	OnResult func(Result)

	// Resume tailoring via the local claude CLI
	TailorResumes     bool   // generate a position-specific resume PDF before applying
	TailoredOutputDir string // directory for tailored PDFs (default: "tailored_resumes")

	// Simplify extension support
	// ProfileDir is a persistent Firefox profile directory that contains the
	// Simplify extension already installed and authenticated.  Run with
	// --setup to create the profile interactively.
	ProfileDir   string
	SimplifyWait time.Duration // pause after form appears for Simplify to auto-fill (0 = disabled)
}

// Result records the outcome of one application attempt.
type Result struct {
	URL     string
	Company string
	Title   string
	// Status is "applied", "dry-run", "skipped", "pending", or "error".
	Status string
	Error  error
	// Requeue signals Run() to push this URL back onto the work queue instead
	// of finalising it.  Used when a temporary blocker (e.g. email verification
	// cooldown) means the URL should be retried later in the same session.
	Requeue bool
}

// Applicator drives browser-based job applications from a slice of URLs.
type Applicator struct {
	cfg        Config
	browser    *Browser
	cooldownMu sync.Mutex
	lastRun    map[string]time.Time // platform → time we last started an application
}

// New creates an Applicator and launches the underlying browser.
// Call Close() when finished.
func New(cfg Config) (*Applicator, error) {
	if cfg.Hold {
		cfg.Headful = true // hold mode is meaningless without a visible window
	}
	if cfg.TailoredOutputDir == "" {
		cfg.TailoredOutputDir = "tailored_resumes"
	}
	if err := os.MkdirAll(cfg.TailoredOutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create tailored-resume dir: %w", err)
	}

	// Fill any empty ApplicantInfo fields from the CV before opening the browser
	// so every field is available to the form-fillers on all platforms.
	if cfg.ResumePath != "" {
		enrichFromResume(&cfg)
	}

	// Bake in defaults for all compensation/availability fields so forms are
	// always filled without requiring the caller to specify values.
	a := &cfg.Applicant
	if a.ExpectedSalary == "" {
		a.ExpectedSalary = "Negotiable"
	}
	if a.NoticePeriod == "" {
		a.NoticePeriod = "2 weeks"
	}
	if a.EarliestStartDate == "" {
		a.EarliestStartDate = time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	}
	if a.CoverLetter == "" {
		a.CoverLetter = "I am excited about this opportunity and believe my background and experience make me a strong fit for this role. I look forward to the chance to contribute and grow with your team."
	}

	if cfg.ProfileDir != "" {
		if err := os.MkdirAll(cfg.ProfileDir, 0o755); err != nil {
			return nil, fmt.Errorf("create profile directory %q: %w", cfg.ProfileDir, err)
		}
	}
	conc := cfg.Concurrency
	if conc < 1 {
		conc = 1
	}
	b, err := NewBrowser(cfg.Headful, cfg.ProfileDir, conc)
	if err != nil {
		return nil, fmt.Errorf("browser init: %w", err)
	}
	return &Applicator{cfg: cfg, browser: b, lastRun: make(map[string]time.Time)}, nil
}

// enrichFromResume parses the CV PDF and back-fills any ApplicantInfo fields
// that the user has not explicitly supplied.  Failures are logged but never
// fatal — missing CV data just means some form fields may be left blank.
func enrichFromResume(cfg *Config) {
	text, err := resume.ExtractText(cfg.ResumePath)
	if err != nil {
		log.Printf("[cv] could not read CV for field extraction: %v", err)
		return
	}
	f := resume.ParseFields(text)
	a := &cfg.Applicant

	if a.City == "" {
		a.City = f.City
	}
	if a.State == "" {
		a.State = f.State
	}
	if a.ZipCode == "" {
		a.ZipCode = f.ZipCode
	}
	if a.Country == "" {
		a.Country = f.Country
	}
	if a.GitHubURL == "" {
		a.GitHubURL = f.GitHub
	}
	if a.Website == "" {
		a.Website = f.Website
	}
	if a.School == "" {
		a.School = f.School
	}
	if a.Degree == "" {
		a.Degree = f.Degree
	}
	if a.FieldOfStudy == "" {
		a.FieldOfStudy = f.FieldOfStudy
	}

	log.Printf("[cv] fields extracted — city=%q state=%q zip=%q country=%q github=%v website=%v school=%q degree=%q field=%q",
		a.City, a.State, a.ZipCode, a.Country, a.GitHubURL != "", a.Website != "",
		a.School, a.Degree, a.FieldOfStudy)
}

// Close releases the browser process.
func (a *Applicator) Close() { a.browser.Close() }

// Run processes each URL and returns one Result per URL.
// Concurrency is capped at cfg.Concurrency (minimum 1).
//
// URLs are grouped by (platform, company) — the same boundary that determines
// whether two applications would collide.  Each group is handled by exactly one
// goroutine that processes its URLs serially, so no two workers ever race on the
// same platform+company pair and no requeuing is needed.  A shared semaphore
// caps the total number of in-flight browser pages at cfg.Concurrency.
func (a *Applicator) Run(ctx context.Context, urls []string) []Result {
	conc := a.cfg.Concurrency
	if conc < 1 {
		conc = 1
	}

	results := make([]Result, len(urls))
	if len(urls) == 0 {
		return results
	}

	type group struct {
		platform string
		indices  []int
	}
	groupMap := make(map[string]*group)
	var groupOrder []string
	for i, u := range urls {
		k := groupKey(u)
		if _, ok := groupMap[k]; !ok {
			groupMap[k] = &group{platform: detectPlatform(u)}
			groupOrder = append(groupOrder, k)
		}
		groupMap[k].indices = append(groupMap[k].indices, i)
	}

	var outstanding atomic.Int64
	outstanding.Store(int64(len(urls)))
	allDone := make(chan struct{})

	completeOne := func(idx int, r Result) {
		results[idx] = r
		if r.Status != "pending" && a.cfg.OnResult != nil {
			a.cfg.OnResult(r)
		}
		if outstanding.Add(-1) == 0 {
			close(allDone)
		}
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for _, k := range groupOrder {
		g := groupMap[k]
		wg.Add(1)
		go func(g *group) {
			defer wg.Done()
			for _, idx := range g.indices {
				completeOne(idx, a.runOne(ctx, urls[idx], g.platform, sem))
			}
		}(g)
	}

	<-allDone
	wg.Wait()
	return results
}

// groupKey returns the scheduling-group key for a URL: the (platform, company)
// pair that determines whether two applications could collide.  URLs with the
// same key are always processed serially by a single goroutine.
func groupKey(rawURL string) string {
	p := detectPlatform(rawURL)
	if p == "generic" {
		u, err := url.Parse(rawURL)
		if err == nil {
			return "generic:" + u.Hostname()
		}
		return "generic"
	}
	if company := companyFromURL(rawURL); company != "" {
		return p + ":" + company
	}
	return p
}

// runOne processes a single URL, blocking on cooldowns and the global
// concurrency semaphore before calling processOne.  When processOne returns
// Requeue (email-verification cooldown), runOne loops — processOne has already
// stamped lastRun with the appropriate offset, so the cooldown wait at the top
// of the loop sleeps the right duration before retrying.
//
// Cooldown keying:
//   - Per-group key (e.g. "greenhouse:grafanalabs"): rate-limits consecutive
//     submissions to the same company.  Stamped here after each attempt.
//   - Platform key (e.g. "greenhouse"): freeze overrides only — set by
//     processOne on captcha or email-verification events that must block every
//     company on the same ATS.  Never stamped here, so different companies on
//     the same platform can run fully in parallel.
func (a *Applicator) runOne(ctx context.Context, rawURL, platform string, sem chan struct{}) Result {
	gk := groupKey(rawURL)
	for {
		if ctx.Err() != nil {
			return Result{URL: rawURL, Status: "pending"}
		}

		if cd, ok := platformCooldowns[platform]; ok {
			for {
				a.cooldownMu.Lock()
				// Per-group: rate-limit this (platform, company) pair.
				waitGroup := cd - time.Since(a.lastRun[gk])
				// Platform-wide: catch email-verify / captcha freezes set by
				// processOne that must stall every company on this ATS.
				waitPlatform := cd - time.Since(a.lastRun[platform])
				wait := waitGroup
				if waitPlatform > wait {
					wait = waitPlatform
				}
				a.cooldownMu.Unlock()
				if wait <= 0 {
					break
				}
				log.Printf("[apply] %s cooldown — waiting %.0fs before %s", platform, wait.Seconds(), rawURL)
				select {
				case <-ctx.Done():
					return Result{URL: rawURL, Status: "pending"}
				case <-time.After(wait):
				}
			}
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return Result{URL: rawURL, Status: "pending"}
		}

		if _, ok := platformCooldowns[platform]; ok {
			a.cooldownMu.Lock()
			a.lastRun[gk] = time.Now() // stamp group key only; platform key reserved for freeze overrides
			a.cooldownMu.Unlock()
		}

		r := a.processOne(ctx, rawURL)
		<-sem

		if !r.Requeue || ctx.Err() != nil {
			if r.Requeue {
				return Result{URL: rawURL, Status: "pending"}
			}
			return r
		}
		// Requeue: processOne set lastRun[platform] with the freeze offset;
		// loop back so the platform-wide wait fires before retrying.
	}
}

func (a *Applicator) processOne(ctx context.Context, rawURL string) Result {
	res := Result{URL: rawURL}

	job, err := ParseJobPage(rawURL)
	if err != nil {
		log.Printf("[apply] %s: parse error: %v", rawURL, err)
		res.Status = "error"
		res.Error = err
		return res
	}
	res.Company = job.Company
	res.Title = job.Title

	log.Printf("[apply] %q @ %s  platform=%s", job.Title, job.Company, job.ATSPlatform)

	// Determine which resume PDF to upload.
	resumePath := a.cfg.ResumePath
	if a.cfg.TailorResumes {
		resumePath = a.tailorResume(ctx, job)
	}

	flags := FillFlags{
		DryRun:       a.cfg.DryRun,
		Screenshot:   a.cfg.Screenshots,
		Headful:      a.cfg.Headful,
		Hold:         a.cfg.Hold,
		SimplifyWait: a.cfg.SimplifyWait,
	}
	if err := a.browser.FillApplication(ctx, job, a.cfg.Applicant, resumePath, flags); err != nil {
		if errors.Is(err, ErrWindowClosed) {
			res.Status = "skipped"
			return res
		}
		// Context cancelled mid-fill — URL was partially or not attempted;
		// treat as pending so it is not written to failed_urls.txt.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{URL: rawURL, Status: "pending"}
		}
		if errors.Is(err, ErrNoApplicationForm) {
			// Permanent: job page has no form (expired, removed, or unsupported
			// platform).  Skip without writing to failed_urls.txt so the URL is
			// not retried on every subsequent run.
			res.Status = "skipped"
			return res
		}
		if errors.Is(err, ErrCaptcha) {
			// Generic forms are one-off pages from unrelated companies, so a
			// cooldown would only penalise unrelated URLs — skip it.
			if job.ATSPlatform != "generic" {
				const captchaCooldown = 10 * time.Minute
				a.cooldownMu.Lock()
				a.lastRun[job.ATSPlatform] = time.Now().Add(captchaCooldown - platformCooldowns[job.ATSPlatform])
				a.cooldownMu.Unlock()
				log.Printf("[apply] %s captcha — enforcing %v cooldown before next %s application",
					job.ATSPlatform, captchaCooldown, job.ATSPlatform)
			} else {
				log.Printf("[apply] captcha on generic form — skipping URL, no cooldown")
			}
			res.Status = "error"
			res.Error = err
			return res
		}
		if errors.Is(err, ErrEmailVerification) {
			// Greenhouse sent a one-time/2FA code to the applicant's email.
			// We cannot enter it programmatically; enforce a 65-minute cooldown
			// so ALL remaining Greenhouse URLs are delayed, then re-queue this
			// URL so it is retried automatically once the cooldown expires.
			const emailVerifyCooldown = 65 * time.Minute
			a.cooldownMu.Lock()
			a.lastRun[job.ATSPlatform] = time.Now().Add(emailVerifyCooldown - platformCooldowns[job.ATSPlatform])
			a.cooldownMu.Unlock()
			log.Printf("[apply] %s email/2FA verification — enforcing %v cooldown, re-queuing %s",
				job.ATSPlatform, emailVerifyCooldown, rawURL)
			return Result{URL: rawURL, Requeue: true}
		}
		log.Printf("[apply] %s: fill error: %v", rawURL, err)
		res.Status = "error"
		res.Error = err
		return res
	}

	if a.cfg.DryRun {
		res.Status = "dry-run"
	} else {
		res.Status = "applied"
	}
	return res
}

// tailorResume extracts the base resume text, calls Claude to tailor it to
// the job, generates a new PDF, and returns its path.  On any error it logs a
// warning and returns the original base resume path so the apply can continue.
func (a *Applicator) tailorResume(ctx context.Context, job *JobInfo) string {
	base := a.cfg.ResumePath

	baseText, err := resume.ExtractText(base)
	if err != nil {
		log.Printf("[tailor] could not read base resume: %v — using original", err)
		return base
	}

	desc := job.Description
	if desc == "" {
		log.Printf("[tailor] no job description scraped for %s — using original resume", job.URL)
		return base
	}

	log.Printf("[tailor] calling Claude to tailor resume for %q @ %s", job.Title, job.Company)
	tailored, err := TailorResume(ctx, baseText, desc, job.Title, job.Company)
	if errors.Is(err, errNoJobDescription) {
		log.Printf("[tailor] job description too thin for %s — using original resume", job.URL)
		return base
	}
	if err != nil {
		log.Printf("[tailor] Claude error: %v — using original resume", err)
		return base
	}

	outName := safeFilename(job.Company+"_"+job.Title) + ".pdf"
	outPath := filepath.Join(a.cfg.TailoredOutputDir, outName)
	if err := resume.GeneratePDF(tailored, outPath); err != nil {
		log.Printf("[tailor] PDF generation failed: %v — using original resume", err)
		return base
	}

	log.Printf("[tailor] tailored resume saved → %s", outPath)
	return outPath
}
