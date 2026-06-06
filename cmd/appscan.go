package cmd

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"syscall"

	"Resume_Contacts_Scraper/internal/applylog"
	"Resume_Contacts_Scraper/internal/output"
	"Resume_Contacts_Scraper/internal/scraper"
)

const appOutputFile = "application_pages.txt"

// defaultTechRoles is the built-in role-keyword list used by the pages command
// when the user does not pass --roles.  A job-application link is followed only
// when its anchor text (the job title on ATS listing pages) contains at least
// one keyword as a case-insensitive substring.  Pass --roles "" to disable.
var defaultTechRoles = []string{
	// Core engineering
	"engineer", "developer", "programmer",
	// Scope / stack
	"software", "backend", "frontend", "fullstack", "full stack", "full-stack",
	// Infrastructure / reliability
	"devops", "sre", "site reliability", "platform", "infrastructure", "cloud",
	// Data / ML / AI
	"data", "machine learning", "deep learning", "mlops", "ai engineer", "ml engineer",
	// Security
	"security", "appsec", "cybersecurity", "devsecops",
	// Mobile / embedded
	"mobile", "android", "ios", "embedded", "firmware",
	// Quality
	"qa", "quality assurance", "sdet", "test automation", "test engineer",
	// Seniority / leadership flavours that imply engineering
	"architect", "tech lead", "technical lead", "staff engineer", "principal engineer",
}

func appscan(f runFlags) {
	cfg := scraper.DefaultConfig
	cfg.Parallelism = f.concurrency
	cfg.ExtraSeeds = mustLoadSeeds(f.seedsFile)
	cfg.Countries = f.countries
	cfg.IgnoreCountries = f.ignoreCountries

	// Role filter: use the user-supplied list when --roles was explicitly passed,
	// otherwise fall back to the built-in tech keyword list.
	// Passing --roles "" disables filtering entirely (f.rolesSet=true, f.roles=nil).
	if f.rolesSet {
		cfg.Roles = f.roles // may be nil → no filter
	} else {
		cfg.Roles = defaultTechRoles
	}
	cfg.BlockedDomains = f.blockedDomains

	fmt.Println("Starting Application Page Scanner...")
	countriesLabel := "all"
	if len(f.countries) > 0 {
		countriesLabel = strings.Join(f.countries, ",")
	}
	ignoreLabel := "none"
	if len(f.ignoreCountries) > 0 {
		ignoreLabel = strings.Join(f.ignoreCountries, ",")
	}
	rolesLabel := "off"
	if len(cfg.Roles) > 0 {
		rolesLabel = fmt.Sprintf("%d keywords", len(cfg.Roles))
	}
	blockedLabel := "none"
	if len(cfg.BlockedDomains) > 0 {
		blockedLabel = fmt.Sprintf("%d domain(s)", len(cfg.BlockedDomains))
	}
	fmt.Printf("Concurrency: %d  |  Extra seeds: %d  |  Countries: %s  |  Ignore: %s  |  Role filter: %s  |  Blocked: %s  |  Output: %s\n\n",
		f.concurrency, len(cfg.ExtraSeeds), countriesLabel, ignoreLabel, rolesLabel, blockedLabel, appOutputFile)

	writer, err := output.NewURLWriter(appOutputFile)
	if err != nil {
		log.Fatalf("failed to open output file: %v", err)
	}
	defer writer.Close()

	// Pre-seed the seen set with URLs already applied to so the scraper never
	// adds them back to application_pages.txt.
	if recs, err := applylog.ReadAll(defaultCompactLogFile); err == nil && len(recs) > 0 {
		var applied []string
		for _, r := range applylog.DeduplicateByURL(recs) {
			if r.Status == "applied" {
				applied = append(applied, r.URL)
			}
		}
		if len(applied) > 0 {
			writer.MarkSeen(applied...)
			fmt.Printf("Excluding %d already-applied URL(s) from output.\n", len(applied))
		}
	}

	scanner := scraper.NewAppScanner(cfg, func(u string) {
		ok, err := writer.Write(u)
		if err != nil {
			log.Printf("write error: %v", err)
			return
		}
		if ok {
			fmt.Printf("[+] %s\n", u)
		}
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := scanner.Run(ctx); err != nil {
		log.Fatalf("scanner error: %v", err)
	}

	if ctx.Err() != nil {
		fmt.Printf("\nInterrupted. %d application pages written to %s\n", writer.Count(), appOutputFile)
		return
	}
	fmt.Printf("\nDone. %d application pages written to %s\n", writer.Count(), appOutputFile)
}
