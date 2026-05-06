package applier

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"Resume_Contacts_Scraper/internal/resume"
)

// ApplicantInfo holds the personal details typed into application forms.
type ApplicantInfo struct {
	Name        string
	Email       string
	Phone       string
	LinkedInURL string
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
	cfg     Config
	browser *Browser
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
	b, err := NewBrowser(cfg.Headful)
	if err != nil {
		return nil, fmt.Errorf("browser init: %w", err)
	}
	return &Applicator{cfg: cfg, browser: b}, nil
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

	// Determine which resume PDF to upload.
	resumePath := a.cfg.ResumePath
	if a.cfg.TailorResumes {
		resumePath = a.tailorResume(ctx, job)
	}

	flags := FillFlags{DryRun: a.cfg.DryRun, Screenshot: a.cfg.Screenshots, Headful: a.cfg.Headful, Hold: a.cfg.Hold}
	if err := a.browser.FillApplication(ctx, job, a.cfg.Applicant, resumePath, flags); err != nil {
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
