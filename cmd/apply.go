package cmd

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

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

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		urlsFile   string
		resumePath string
		name       string
		email      string
		phone      string
		linkedin   string
		dryRun     bool
		hold       bool
		conc       int
		headful    bool
		shots      bool
		tailor    bool
		outputDir string
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

	_ = fs.Parse(os.Args[2:])

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

	urls, err := readLines(urlsFile)
	if err != nil {
		log.Fatalf("load URLs: %v", err)
	}
	if len(urls) == 0 {
		fmt.Println("No URLs found in the file — nothing to do.")
		return
	}

	cfg := applier.Config{
		Applicant: applier.ApplicantInfo{
			Name:        name,
			Email:       email,
			Phone:       phone,
			LinkedInURL: linkedin,
		},
		ResumePath:        resumePath,
		DryRun:            dryRun,
		Hold:              hold,
		Concurrency:       conc,
		Headful:           headful,
		Screenshots:       shots,
		TailorResumes:     tailor,
		TailoredOutputDir: outputDir,
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

	var applied, dryCnt, errCnt int
	for _, r := range results {
		switch r.Status {
		case "applied":
			applied++
			fmt.Printf("[+] %s @ %s\n", r.Title, r.Company)
		case "dry-run":
			dryCnt++
			fmt.Printf("[~] %s @ %s  (dry-run)\n", r.Title, r.Company)
		default:
			errCnt++
			fmt.Printf("[!] %s: %v\n", r.URL, r.Error)
		}
	}
	fmt.Printf("\nDone. Applied: %d  Dry-run: %d  Errors: %d\n", applied, dryCnt, errCnt)
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
