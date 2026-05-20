package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type tuiSavedState struct {
	Start    tuiStartSaved    `json:"start"`
	Pages    tuiPagesSaved    `json:"pages"`
	Discover tuiDiscoverSaved `json:"discover"`
	Apply    tuiApplySaved    `json:"apply"`
	Clean    tuiCleanSaved    `json:"clean"`
}

type tuiStartSaved struct {
	Concurrency string `json:"concurrency"`
	Seeds       string `json:"seeds"`
	Countries   string `json:"countries"`
	SMTPVerify  bool   `json:"smtp_verify"`
}

type tuiPagesSaved struct {
	Concurrency string `json:"concurrency"`
	Seeds       string `json:"seeds"`
	Countries   string `json:"countries"`
	SMTPVerify  bool   `json:"smtp_verify"`
}

type tuiDiscoverSaved struct {
	Concurrency string `json:"concurrency"`
	Seeds       string `json:"seeds"`
	Countries   string `json:"countries"`
	Hops        string `json:"hops"`
}

type tuiApplySaved struct {
	Mode           int    `json:"mode"`
	URLsFile       string `json:"urls_file"`
	Resume         string `json:"resume"`
	CoverLetter    string `json:"cover_letter"`
	Name           string `json:"name"`
	Email          string `json:"email"`
	Phone          string `json:"phone"`
	LinkedIn       string `json:"linkedin"`
	GitHub         string `json:"github"`
	Website        string `json:"website"`
	City           string `json:"city"`
	State          string `json:"state"`
	ZIP            string `json:"zip"`
	Country        string `json:"country"`
	School         string `json:"school"`
	Degree         string `json:"degree"`
	FieldOfStudy   string `json:"field_of_study"`
	Salary         string `json:"salary"`
	NoticePeriod   string `json:"notice_period"`
	StartDate      string `json:"start_date"`
	WorkAuthIdx    int    `json:"work_auth_idx"`
	SponsorshipIdx int    `json:"sponsorship_idx"`
	GenderIdx      int    `json:"gender_idx"`
	EthnicityIdx   int    `json:"ethnicity_idx"`
	Headful        bool   `json:"headful"`
	DryRun         bool   `json:"dry_run"`
	Hold           bool   `json:"hold"`
	Screenshots    bool   `json:"screenshots"`
	Tailor         bool   `json:"tailor"`
	Concurrency    string `json:"concurrency"`
	UseSimplify    bool   `json:"use_simplify"`
	SimplifyWait   string `json:"simplify_wait"`
	OutputDir      string `json:"output_dir"`
	FailedURLs     string `json:"failed_urls"`
	LogFile        string `json:"log_file"`
}

type tuiCleanSaved struct {
	Directory string `json:"directory"`
	Regex     string `json:"regex"`
	Dedup     bool   `json:"dedup"`
}

func tuiStatePath() string {
	base, err := os.UserConfigDir()
	if err != nil {
		if home := os.Getenv("HOME"); home != "" {
			base = filepath.Join(home, ".config")
		} else {
			base = "."
		}
	}
	return filepath.Join(base, "resume-contacts-scraper", "tui_state.json")
}

// tuiDefaultState returns the hardcoded first-run defaults.
// Seeds and resume paths are intentionally left empty here; the caller
// fills them with disk-detected values after merging.
func tuiDefaultState() tuiSavedState {
	return tuiSavedState{
		Start: tuiStartSaved{
			Concurrency: "8",
			Countries:   "de,il,us,ca,uk,pt,cz,at",
		},
		Pages: tuiPagesSaved{
			Concurrency: "16",
			Countries:   "de,il,us,ca,uk,pt,cz,at",
			SMTPVerify:  true,
		},
		Discover: tuiDiscoverSaved{
			Concurrency: "40",
			Countries:   "de,il,us,ca,uk,pt,cz,at",
			Hops:        "6",
		},
		Apply: tuiApplySaved{
			URLsFile:       "application_pages.txt",
			Name:           "Wael Karram",
			Email:          "wael@karram.work",
			NoticePeriod:   "2 weeks",
			StartDate:      time.Now().AddDate(0, 0, 14).Format("2006-01-02"),
			SponsorshipIdx: 1, // "no" is index 1 in {no, yes}... wait, it's {no,yes} so 0=no
			Headful:        true,
			Concurrency:    "1",
			SimplifyWait:   "15",
			OutputDir:      "tailored_resumes",
			FailedURLs:     "failed_urls.txt",
			LogFile:        "apply.log",
		},
		Clean: tuiCleanSaved{
			Directory: "contacts",
			Dedup:     true,
		},
	}
}

func loadTUIState() tuiSavedState {
	def := tuiDefaultState()

	data, err := os.ReadFile(tuiStatePath())
	if err != nil {
		return def
	}
	var saved tuiSavedState
	if err := json.Unmarshal(data, &saved); err != nil {
		return def
	}

	// Keep the start date fresh: if it's empty or already in the past, reset it.
	if saved.Apply.StartDate == "" {
		saved.Apply.StartDate = def.Apply.StartDate
	} else if t, err := time.Parse("2006-01-02", saved.Apply.StartDate); err != nil || !t.After(time.Now()) {
		saved.Apply.StartDate = def.Apply.StartDate
	}

	return saved
}

func saveTUIState(s tuiSavedState) {
	path := tuiStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
