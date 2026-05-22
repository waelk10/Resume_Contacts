package cmd

import (
	"flag"
	"fmt"
	"log"
	"os"

	"Resume_Contacts_Scraper/internal/applier"
)

func compactProfiles() {
	fs := flag.NewFlagSet("compact-profiles", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: Resume_Contacts_Scraper compact-profiles [flags]

Removes cached and ephemeral data from a Firefox profile directory and all
numbered clones (profile-0, profile-1, …) that were created for concurrent
apply runs.  Extension data, cookies, and saved passwords are preserved so
the Simplify extension stays authenticated.

What is removed:
  cache2/  cache/  startupCache/  thumbnails/  crashes/  datareporting/
  minidumps/  saved-telemetry-pings/  sessionstore-backups/
  storage/temporary/  sessionstore.jsonlz4  sessionCheckpoints.json

Flags:
`)
		fs.PrintDefaults()
	}

	var profileDir string
	fs.StringVar(&profileDir, "profile-dir", defaultProfileDir(),
		"Firefox profile directory (base; clones are detected automatically)")

	_ = fs.Parse(os.Args[2:])

	if profileDir == "" {
		fmt.Fprintln(os.Stderr, "error: --profile-dir is required")
		fs.Usage()
		os.Exit(1)
	}

	// Collect the base profile and any numbered clones.
	dirs := collectProfileDirs(profileDir)
	if len(dirs) == 0 {
		fmt.Printf("No profiles found at %s\n", profileDir)
		return
	}

	var totalFreed int64
	for _, dir := range dirs {
		freed, err := applier.CompactProfile(dir)
		if err != nil {
			log.Printf("warning: compact %s: %v", dir, err)
			continue
		}
		fmt.Printf("  %-60s  freed %s\n", dir, formatBytes(freed))
		totalFreed += freed
	}
	fmt.Printf("\nTotal freed: %s\n", formatBytes(totalFreed))
}

// collectProfileDirs returns the base profileDir (if it exists) followed by
// any numbered clones (profileDir-0, profileDir-1, …) found on disk.
func collectProfileDirs(base string) []string {
	var dirs []string
	if _, err := os.Stat(base); err == nil {
		dirs = append(dirs, base)
	}
	for i := 0; ; i++ {
		clone := fmt.Sprintf("%s-%d", base, i)
		if _, err := os.Stat(clone); err != nil {
			break
		}
		dirs = append(dirs, clone)
	}
	return dirs
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// defaultProfileDir is defined in apply.go and shared within package cmd.
