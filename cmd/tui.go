package cmd

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"Resume_Contacts_Scraper/internal/applylog"
)

func runTUI() {
	st := loadTUIState()

	// Fill seed / resume paths with disk-detected defaults when the saved
	// state has an empty value (e.g. on first run or after a purge).
	if st.Start.Seeds == "" {
		st.Start.Seeds = tuiSeedsDefault()
	}
	if st.Pages.Seeds == "" {
		st.Pages.Seeds = tuiSeedsDefault()
	}
	if st.Discover.Seeds == "" {
		st.Discover.Seeds = tuiSeedsDefault()
	}
	if st.Apply.Resume == "" {
		st.Apply.Resume = tuiResumeDefault()
	}

	app := tview.NewApplication()
	var pendingAction func()

	rightPages := tview.NewPages()
	cmdList := tview.NewList().ShowSecondaryText(false)
	focusList := func() { app.SetFocus(cmdList) }

	addCmd := func(name string, f *tview.Form) {
		cmdList.AddItem(name, "", 0, nil)
		f.SetBorder(true).
			SetTitle(" "+name+" ").
			SetTitleAlign(tview.AlignLeft).
			SetBorderColor(tcell.ColorDarkCyan)
		f.SetCancelFunc(focusList)
		rightPages.AddPage(name, f, true, false)
	}

	// ── Lifted form variables ────────────────────────────────────────────
	// All sections live here so snapshot() can read them regardless of
	// which Run button the user clicks.

	// start
	startConc := st.Start.Concurrency
	startSeeds := st.Start.Seeds
	startCountries := st.Start.Countries
	startIgnoreCountries := st.Start.IgnoreCountries
	startSMTP := st.Start.SMTPVerify

	// pages
	pgConc := st.Pages.Concurrency
	pgSeeds := st.Pages.Seeds
	pgCountries := st.Pages.Countries
	pgIgnoreCountries := st.Pages.IgnoreCountries
	pgRoles := st.Pages.Roles
	pgBlocked := st.Pages.BlockedDomains

	// discover
	discConc := st.Discover.Concurrency
	discSeeds := st.Discover.Seeds
	discCountries := st.Discover.Countries
	discIgnoreCountries := st.Discover.IgnoreCountries
	discHops := st.Discover.Hops

	// apply
	applyMode := st.Apply.Mode
	applyURLs := st.Apply.URLsFile
	applyResume := st.Apply.Resume
	applyCoverLetter := st.Apply.CoverLetter
	applyName := st.Apply.Name
	applyEmail := st.Apply.Email
	applyPhone := st.Apply.Phone
	applyLinkedIn := st.Apply.LinkedIn
	applyGitHub := st.Apply.GitHub
	applyWebsite := st.Apply.Website
	applyCity := st.Apply.City
	applyState := st.Apply.State
	applyZIP := st.Apply.ZIP
	applyCountry := st.Apply.Country
	applySchool := st.Apply.School
	applyDegree := st.Apply.Degree
	applyFieldOfStudy := st.Apply.FieldOfStudy
	applySalary := st.Apply.Salary
	applyNoticePeriod := st.Apply.NoticePeriod
	applyStartDate := st.Apply.StartDate
	applyWorkAuthIdx := st.Apply.WorkAuthIdx
	applySponsorIdx := st.Apply.SponsorshipIdx
	applyGenderIdx := st.Apply.GenderIdx
	applyEthnicityIdx := st.Apply.EthnicityIdx
	applyHeadful := st.Apply.Headful
	applyDryRun := st.Apply.DryRun
	applyHold := st.Apply.Hold
	applyShots := st.Apply.Screenshots
	applyTailor := st.Apply.Tailor
	applyConc := st.Apply.Concurrency
	applySimplify := st.Apply.UseSimplify
	applySwait := st.Apply.SimplifyWait
	applyOutputDir := st.Apply.OutputDir
	applyFailedURLs := st.Apply.FailedURLs
	applyLogFile := st.Apply.LogFile

	// clean
	cleanDir := st.Clean.Directory
	cleanRegex := st.Clean.Regex
	cleanDedup := st.Clean.Dedup

	// snapshot captures the current value of every form variable.
	snapshot := func() tuiSavedState {
		return tuiSavedState{
			Start:    tuiStartSaved{startConc, startSeeds, startCountries, startIgnoreCountries, startSMTP},
			Pages:    tuiPagesSaved{pgConc, pgSeeds, pgCountries, pgIgnoreCountries, pgRoles, pgBlocked},
			Discover: tuiDiscoverSaved{discConc, discSeeds, discCountries, discIgnoreCountries, discHops},
			Apply: tuiApplySaved{
				Mode: applyMode, URLsFile: applyURLs, Resume: applyResume,
				CoverLetter: applyCoverLetter, Name: applyName, Email: applyEmail,
				Phone: applyPhone, LinkedIn: applyLinkedIn, GitHub: applyGitHub,
				Website: applyWebsite, City: applyCity, State: applyState,
				ZIP: applyZIP, Country: applyCountry,
				School: applySchool, Degree: applyDegree, FieldOfStudy: applyFieldOfStudy,
				Salary: applySalary,
				NoticePeriod: applyNoticePeriod, StartDate: applyStartDate,
				WorkAuthIdx: applyWorkAuthIdx, SponsorshipIdx: applySponsorIdx,
				GenderIdx: applyGenderIdx, EthnicityIdx: applyEthnicityIdx,
				Headful: applyHeadful, DryRun: applyDryRun, Hold: applyHold,
				Screenshots: applyShots, Tailor: applyTailor, Concurrency: applyConc,
				UseSimplify: applySimplify, SimplifyWait: applySwait, OutputDir: applyOutputDir,
				FailedURLs: applyFailedURLs, LogFile: applyLogFile,
			},
			Clean: tuiCleanSaved{cleanDir, cleanRegex, cleanDedup},
		}
	}

	// ── start ──────────────────────────────────────────────────────────────
	{
		f := tview.NewForm().
			AddInputField("Concurrency", startConc, 6, acceptInt, func(t string) { startConc = t }).
			AddInputField("Seeds file", startSeeds, 40, nil, func(t string) { startSeeds = t }).
			AddInputField("Countries", startCountries, 40, nil, func(t string) { startCountries = t }).
			AddInputField("Ignore countries", startIgnoreCountries, 40, nil, func(t string) { startIgnoreCountries = t }).
			AddCheckbox("SMTP-verify each address", startSMTP, func(c bool) { startSMTP = c }).
			AddButton("Run", func() {
				saveTUIState(snapshot())
				pendingAction = func() {
					startup(tuiBuildFlags(startConc, startSeeds, startCountries, startIgnoreCountries, 0, startSMTP))
				}
				app.Stop()
			})
		addCmd("start", f)
	}

	// ── pages ──────────────────────────────────────────────────────────────
	{
		f := tview.NewForm().
			AddInputField("Concurrency", pgConc, 6, acceptInt, func(t string) { pgConc = t }).
			AddInputField("Seeds file", pgSeeds, 40, nil, func(t string) { pgSeeds = t }).
			AddInputField("Countries", pgCountries, 40, nil, func(t string) { pgCountries = t }).
			AddInputField("Ignore countries", pgIgnoreCountries, 40, nil, func(t string) { pgIgnoreCountries = t }).
			AddInputField("Role keywords, space or comma-separated (empty = tech default, * = all)", pgRoles, 60, nil, func(t string) { pgRoles = t }).
			AddInputField("Blocked domains, space or comma-separated (e.g. amazon.com google.com)", pgBlocked, 60, nil, func(t string) { pgBlocked = t }).
			AddButton("Run", func() {
				saveTUIState(snapshot())
				pendingAction = func() {
					flags := tuiBuildFlags(pgConc, pgSeeds, pgCountries, pgIgnoreCountries, 0, false)
					// Non-empty field overrides the built-in tech default.
					// "*" or "all" disables the filter entirely.
					if pgRoles != "" {
						flags.rolesSet = true
						raw := pgRoles
						if raw != "*" && raw != "all" {
							for _, tok := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
								tok = strings.ToLower(tok)
								if tok != "" {
									flags.roles = append(flags.roles, tok)
								}
							}
						}
						// else: flags.roles stays nil → no filter
					}
					for _, tok := range strings.FieldsFunc(pgBlocked, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
						tok = strings.ToLower(tok)
						if tok != "" {
							flags.blockedDomains = append(flags.blockedDomains, tok)
						}
					}
					appscan(flags)
				}
				app.Stop()
			})
		addCmd("pages", f)
	}

	// ── discover ───────────────────────────────────────────────────────────
	{
		f := tview.NewForm().
			AddInputField("Concurrency", discConc, 6, acceptInt, func(t string) { discConc = t }).
			AddInputField("Seeds file", discSeeds, 40, nil, func(t string) { discSeeds = t }).
			AddInputField("Countries", discCountries, 40, nil, func(t string) { discCountries = t }).
			AddInputField("Ignore countries", discIgnoreCountries, 40, nil, func(t string) { discIgnoreCountries = t }).
			AddInputField("Hops", discHops, 6, acceptInt, func(t string) { discHops = t }).
			AddButton("Run", func() {
				hops, _ := strconv.Atoi(discHops)
				if hops < 0 {
					hops = 0
				}
				saveTUIState(snapshot())
				flags := tuiBuildFlags(discConc, discSeeds, discCountries, discIgnoreCountries, hops, false)
				pendingAction = func() { discoverSeeds(flags) }
				app.Stop()
			})
		addCmd("discover", f)
	}

	// ── apply ──────────────────────────────────────────────────────────────
	{
		workAuthOpts := []string{"yes", "no"}
		sponsorOpts := []string{"no", "yes"}
		genderOpts := []string{"decline", "male", "female", "non-binary"}
		ethnicityOpts := []string{"decline", "white", "black", "hispanic", "asian", "american-indian", "pacific-islander", "two-or-more"}

		f := tview.NewForm().
			// mode
			AddDropDown("Mode", []string{"Run", "Setup (Simplify)"}, applyMode, func(_ string, i int) { applyMode = i }).
			// files
			AddInputField("URLs file", applyURLs, 40, nil, func(t string) { applyURLs = t }).
			AddInputField("Resume PDF", applyResume, 40, nil, func(t string) { applyResume = t }).
			AddInputField("Cover letter (txt, optional)", applyCoverLetter, 40, nil, func(t string) { applyCoverLetter = t }).
			// identity
			AddInputField("Name", applyName, 30, nil, func(t string) { applyName = t }).
			AddInputField("Email", applyEmail, 30, nil, func(t string) { applyEmail = t }).
			AddInputField("Phone", applyPhone, 20, nil, func(t string) { applyPhone = t }).
			AddInputField("LinkedIn URL", applyLinkedIn, 40, nil, func(t string) { applyLinkedIn = t }).
			AddInputField("GitHub URL", applyGitHub, 40, nil, func(t string) { applyGitHub = t }).
			AddInputField("Website URL", applyWebsite, 40, nil, func(t string) { applyWebsite = t }).
			// location
			AddInputField("City", applyCity, 20, nil, func(t string) { applyCity = t }).
			AddInputField("State / province", applyState, 20, nil, func(t string) { applyState = t }).
			AddInputField("ZIP / postal code", applyZIP, 12, nil, func(t string) { applyZIP = t }).
			AddInputField("Country", applyCountry, 20, nil, func(t string) { applyCountry = t }).
			// education (auto-extracted from CV; override here if needed)
			AddInputField("School / university", applySchool, 40, nil, func(t string) { applySchool = t }).
			AddInputField("Degree (bachelor/master/phd/associate)", applyDegree, 20, nil, func(t string) { applyDegree = t }).
			AddInputField("Field of study / major", applyFieldOfStudy, 30, nil, func(t string) { applyFieldOfStudy = t }).
			// compensation
			AddInputField("Expected salary", applySalary, 20, nil, func(t string) { applySalary = t }).
			AddInputField("Notice period", applyNoticePeriod, 20, nil, func(t string) { applyNoticePeriod = t }).
			AddInputField("Earliest start date (YYYY-MM-DD)", applyStartDate, 14, nil, func(t string) { applyStartDate = t }).
			// eligibility
			AddDropDown("Work authorised?", workAuthOpts, applyWorkAuthIdx, func(_ string, i int) { applyWorkAuthIdx = i }).
			AddDropDown("Require sponsorship?", sponsorOpts, applySponsorIdx, func(_ string, i int) { applySponsorIdx = i }).
			// EEO
			AddDropDown("Gender (EEO)", genderOpts, applyGenderIdx, func(_ string, i int) { applyGenderIdx = i }).
			AddDropDown("Ethnicity (EEO)", ethnicityOpts, applyEthnicityIdx, func(_ string, i int) { applyEthnicityIdx = i }).
			// browser
			AddCheckbox("Headful (show browser)", applyHeadful, func(c bool) { applyHeadful = c }).
			AddCheckbox("Dry run (no submit)", applyDryRun, func(c bool) { applyDryRun = c }).
			AddCheckbox("Hold (keep window open)", applyHold, func(c bool) { applyHold = c }).
			AddCheckbox("Screenshots", applyShots, func(c bool) { applyShots = c }).
			AddCheckbox("Tailor resume with Claude", applyTailor, func(c bool) { applyTailor = c }).
			AddInputField("Browser concurrency", applyConc, 4, acceptInt, func(t string) { applyConc = t }).
			// simplify / output
			AddCheckbox("Use Simplify autofill", applySimplify, func(c bool) { applySimplify = c }).
			AddInputField("Simplify wait (s)", applySwait, 6, acceptInt, func(t string) { applySwait = t }).
			AddInputField("Tailored resumes dir", applyOutputDir, 30, nil, func(t string) { applyOutputDir = t }).
			AddInputField("Failed-URLs file", applyFailedURLs, 30, nil, func(t string) { applyFailedURLs = t }).
			AddInputField("Log file", applyLogFile, 30, nil, func(t string) { applyLogFile = t }).
			AddButton("Run", func() {
				saveTUIState(snapshot())
				if applyMode == 1 {
					pendingAction = func() {
						os.Args = []string{os.Args[0], "apply", "--setup"}
						applyJobs()
					}
					app.Stop()
					return
				}
				args := []string{os.Args[0], "apply",
					"--urls", applyURLs,
					"--resume", applyResume,
					"--name", applyName,
					"--email", applyEmail,
					"--notice-period", applyNoticePeriod,
					"--start-date", applyStartDate,
					"--work-auth", workAuthOpts[applyWorkAuthIdx],
					"--sponsorship", sponsorOpts[applySponsorIdx],
					"--gender", genderOpts[applyGenderIdx],
					"--ethnicity", ethnicityOpts[applyEthnicityIdx],
					"--concurrency", applyConc,
					"--output-dir", applyOutputDir,
				}
				if applyCoverLetter != "" {
					args = append(args, "--cover-letter", applyCoverLetter)
				}
				if applyPhone != "" {
					args = append(args, "--phone", applyPhone)
				}
				if applyLinkedIn != "" {
					args = append(args, "--linkedin", applyLinkedIn)
				}
				if applyGitHub != "" {
					args = append(args, "--github", applyGitHub)
				}
				if applyWebsite != "" {
					args = append(args, "--website", applyWebsite)
				}
				if applyCity != "" {
					args = append(args, "--city", applyCity)
				}
				if applyState != "" {
					args = append(args, "--state", applyState)
				}
				if applyZIP != "" {
					args = append(args, "--zip", applyZIP)
				}
				if applyCountry != "" {
					args = append(args, "--country", applyCountry)
				}
				if applySchool != "" {
					args = append(args, "--school", applySchool)
				}
				if applyDegree != "" {
					args = append(args, "--degree", applyDegree)
				}
				if applyFieldOfStudy != "" {
					args = append(args, "--field-of-study", applyFieldOfStudy)
				}
				if applySalary != "" {
					args = append(args, "--salary", applySalary)
				}
				if applyHeadful {
					args = append(args, "--headful")
				}
				if applyDryRun {
					args = append(args, "--dry-run")
				}
				if applyHold {
					args = append(args, "--hold")
				}
				if applyShots {
					args = append(args, "--screenshots")
				}
				if applyTailor {
					args = append(args, "--tailor")
				}
				if applySimplify {
					if w, _ := strconv.Atoi(applySwait); w > 0 {
						args = append(args, "--simplify-wait", applySwait, "--profile-dir", defaultProfileDir())
					}
				}
				if applyFailedURLs != "" {
					args = append(args, "--failed-urls", applyFailedURLs)
				}
				if applyLogFile != "" {
					args = append(args, "--log-file", applyLogFile)
				}
				pendingAction = func() { os.Args = args; applyJobs() }
				app.Stop()
			})
		addCmd("apply", f)
	}

	// ── clean ──────────────────────────────────────────────────────────────
	{
		f := tview.NewForm().
			AddInputField("Directory", cleanDir, 30, nil, func(t string) { cleanDir = t }).
			AddInputField("Filter regex (empty = none)", cleanRegex, 40, nil, func(t string) { cleanRegex = t }).
			AddCheckbox("Deduplicate by email", cleanDedup, func(c bool) { cleanDedup = c }).
			AddButton("Run", func() {
				if cleanRegex == "" && !cleanDedup {
					return
				}
				saveTUIState(snapshot())
				args := []string{os.Args[0], "clean", "--dir", cleanDir}
				if cleanRegex != "" {
					args = append(args, "--filter-regex", cleanRegex)
				}
				if cleanDedup {
					args = append(args, "--dedup")
				}
				pendingAction = func() { os.Args = args; cleanContacts() }
				app.Stop()
			})
		addCmd("clean", f)
	}

	// ── purge ──────────────────────────────────────────────────────────────
	{
		f := tview.NewForm().
			AddTextView("Warning", "Deletes all contacts, application pages,\nand discovered seeds. Cannot be undone.", 0, 2, false, false).
			AddButton("Purge", func() {
				pendingAction = purgeAll
				app.Stop()
			})
		addCmd("purge", f)
	}

	// ── log ────────────────────────────────────────────────────────────────
	var logTable *tview.Table
	var logStatus *tview.TextView
	var logRecords []applylog.Record
	var logFilterStatus string
	var logFilterText string

	logTable = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	logStatus = tview.NewTextView().SetDynamicColors(true)

	logRefresh := func() {
		logTable.Clear()
		cols := []string{"Date", "Status", "Company", "Title", "Platform"}
		for i, h := range cols {
			logTable.SetCell(0, i, tview.NewTableCell(h).
				SetSelectable(false).
				SetAttributes(tcell.AttrBold).
				SetExpansion(1))
		}
		row := 1
		for _, r := range logRecords {
			if logFilterStatus != "" && r.Status != logFilterStatus {
				continue
			}
			if logFilterText != "" {
				needle := strings.ToLower(logFilterText)
				if !strings.Contains(strings.ToLower(r.Company+" "+r.Title+" "+r.URL), needle) {
					continue
				}
			}
			color := tcell.ColorWhite
			switch r.Status {
			case "applied":
				color = tcell.ColorGreen
			case "dry-run":
				color = tcell.ColorAqua
			case "skipped":
				color = tcell.ColorGray
			case "error":
				color = tcell.ColorRed
			}
			date := r.TS.Format("2006-01-02")
			logTable.SetCell(row, 0, tview.NewTableCell(date).SetTextColor(color))
			logTable.SetCell(row, 1, tview.NewTableCell(r.Status).SetTextColor(color))
			logTable.SetCell(row, 2, tview.NewTableCell(r.Company).SetMaxWidth(28).SetTextColor(color))
			logTable.SetCell(row, 3, tview.NewTableCell(r.Title).SetMaxWidth(35).SetTextColor(color))
			logTable.SetCell(row, 4, tview.NewTableCell(r.Platform).SetTextColor(color))
			row++
		}
		shown := row - 1
		logStatus.SetText(fmt.Sprintf(" [yellow]%d[-] / %d records", shown, len(logRecords)))
	}

	logReload := func() {
		recs, _ := applylog.ReadAll(defaultCompactLogFile)
		// Newest first.
		sort.Slice(recs, func(i, j int) bool {
			return recs[i].TS.After(recs[j].TS)
		})
		logRecords = recs
		logRefresh()
	}

	statusOpts := []string{"all", "applied", "dry-run", "skipped", "error"}
	logFilterForm := tview.NewForm().
		AddDropDown("Status", statusOpts, 0, func(s string, _ int) {
			if s == "all" {
				logFilterStatus = ""
			} else {
				logFilterStatus = s
			}
			logRefresh()
		}).
		AddInputField("Search (company/title/URL)", "", 35, nil, func(t string) {
			logFilterText = t
			logRefresh()
		})
	logFilterForm.SetBorder(false)
	logFilterForm.SetCancelFunc(focusList)

	logTable.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			focusList()
			return nil
		}
		if event.Key() == tcell.KeyTab || event.Key() == tcell.KeyBacktab {
			app.SetFocus(logFilterForm)
			return nil
		}
		return event
	})

	logPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(logFilterForm, 5, 0, true).
		AddItem(logTable, 0, 1, false).
		AddItem(logStatus, 1, 0, false)
	logPanel.SetBorder(true).
		SetTitle(" log ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDarkCyan)

	cmdList.AddItem("log", "", 0, nil)
	rightPages.AddPage("log", logPanel, true, false)

	logReload() // initial load

	// Show start by default.
	rightPages.SwitchToPage("start")

	cmdList.SetChangedFunc(func(_ int, main string, _ string, _ rune) {
		rightPages.SwitchToPage(main)
		if main == "log" {
			logReload()
			app.SetFocus(logFilterForm)
		}
	})
	cmdList.SetSelectedFunc(func(_ int, main string, _ string, _ rune) {
		rightPages.SwitchToPage(main)
		app.SetFocus(rightPages)
	})
	cmdList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape ||
			(event.Key() == tcell.KeyRune && event.Rune() == 'q') {
			saveTUIState(snapshot())
			app.Stop()
			return nil
		}
		return event
	})

	// Ctrl+C can arrive while any widget has focus, so handle it globally.
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			saveTUIState(snapshot())
			app.Stop()
			return nil
		}
		return event
	})

	cmdList.
		SetBorder(true).
		SetTitle(" Commands ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDarkCyan)

	body := tview.NewFlex().
		AddItem(cmdList, 14, 0, true).
		AddItem(rightPages, 0, 1, false)

	header := tview.NewTextView().
		SetText(" Resume Contacts Scraper  v" + version).
		SetTextColor(tcell.ColorYellow)

	footer := tview.NewTextView().
		SetText(" ↑↓ select   Enter  focus form   Tab  next field   Esc  back / quit   q  quit")

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 1, 0, false).
		AddItem(body, 0, 1, true).
		AddItem(footer, 1, 0, false)

	if err := app.SetRoot(root, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}

	if pendingAction != nil {
		pendingAction()
	}
}

// acceptInt restricts an input field to digits only.
func acceptInt(text string, _ rune) bool {
	if text == "" {
		return true
	}
	_, err := strconv.Atoi(text)
	return err == nil
}

func tuiBuildFlags(concStr, seeds, countries, ignoreCountries string, hops int, smtpVerify bool) runFlags {
	c, _ := strconv.Atoi(concStr)
	if c < 1 {
		c = 1
	}
	if c > maxConcurrency {
		c = maxConcurrency
	}
	var cs []string
	for _, tok := range strings.Split(countries, ",") {
		tok = strings.TrimSpace(strings.ToLower(tok))
		if tok != "" {
			cs = append(cs, tok)
		}
	}
	var ics []string
	for _, tok := range strings.Split(ignoreCountries, ",") {
		tok = strings.TrimSpace(strings.ToLower(tok))
		if tok != "" {
			ics = append(ics, tok)
		}
	}
	return runFlags{concurrency: c, seedsFile: seeds, countries: cs, ignoreCountries: ics, hops: hops, smtpVerify: smtpVerify}
}

func tuiSeedsDefault() string {
	if _, err := os.Stat(discoveredSeedsFile); err == nil {
		return discoveredSeedsFile
	}
	return ""
}

func tuiResumeDefault() string {
	for _, candidate := range []string{"cv.pdf", "resume.pdf", "CV.pdf"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
