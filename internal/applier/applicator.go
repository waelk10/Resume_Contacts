package applier

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"Resume_Contacts_Scraper/internal/resume"
)


// platformCooldowns is the minimum pause between consecutive applications on
// the same ATS platform.  Ashby's spam filters are sensitive to rapid
// submissions — a per-platform cooldown prevents the account from being flagged
// and applications silently dropped.
var platformCooldowns = map[string]time.Duration{
	"ashby": 90 * time.Second,
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
	// Status is "applied", "dry-run", or "error".
	Status string
	Error  error
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

	if cfg.ProfileDir != "" {
		if err := os.MkdirAll(cfg.ProfileDir, 0o755); err != nil {
			return nil, fmt.Errorf("create profile directory %q: %w", cfg.ProfileDir, err)
		}
	}
	b, err := NewBrowser(cfg.Headful, cfg.ProfileDir)
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

	log.Printf("[cv] fields extracted — city=%q state=%q zip=%q country=%q github=%v website=%v",
		a.City, a.State, a.ZipCode, a.Country, a.GitHubURL != "", a.Website != "")
}

// Close releases the browser process.
func (a *Applicator) Close() { a.browser.Close() }

// Run processes each URL and returns one Result per URL.
// Concurrency is capped at cfg.Concurrency (minimum 1).
func (a *Applicator) Run(ctx context.Context, urls []string) []Result {
	conc := a.cfg.Concurrency
	if conc < 1 {
		conc = 1
	}

	results := make([]Result, len(urls))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, u := range urls {
		wg.Add(1)
		// Launch immediately; each goroutine competes for a semaphore slot or
		// exits right away when the context is cancelled — no blocking in the
		// main loop means Ctrl+C is felt instantly.
		go func(idx int, rawURL string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				results[idx] = a.processOne(ctx, rawURL)
			case <-ctx.Done():
				results[idx] = Result{URL: rawURL, Status: "error", Error: ctx.Err()}
			}
		}(i, u)
	}
	wg.Wait()
	return results
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

	// Enforce per-platform cooldown before starting this application.
	// We hold the lock only for the timestamp check/update; the actual sleep
	// happens without the lock so other platforms aren't blocked.
	if cd, ok := platformCooldowns[job.ATSPlatform]; ok {
		a.cooldownMu.Lock()
		wait := cd - time.Since(a.lastRun[job.ATSPlatform])
		if wait > 0 {
			a.cooldownMu.Unlock()
			log.Printf("[apply] %s cooldown — waiting %.0fs before next application", job.ATSPlatform, wait.Seconds())
			select {
			case <-ctx.Done():
				return Result{URL: rawURL, Status: "error", Error: ctx.Err()}
			case <-time.After(wait):
			}
			a.cooldownMu.Lock()
		}
		a.lastRun[job.ATSPlatform] = time.Now()
		a.cooldownMu.Unlock()
	}

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
