package applier

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tebeka/selenium"
	"github.com/tebeka/selenium/firefox"
)

// FillFlags controls form-fill behaviour for a single application.
type FillFlags struct {
	DryRun       bool          // fill fields but do not click Submit
	Screenshot   bool          // save a PNG after filling
	Headful      bool          // browser is visible — pause on dry-run so user can inspect form
	Hold         bool          // keep window open until user closes it, then move to next URL
	SimplifyWait time.Duration // pause after form appears to let the Simplify extension auto-fill (0 = disabled)
}

// pooledSession owns one geckodriver process + one Firefox session.
// Keeping the service per-slot lets Close() stop every geckodriver, and lets
// returnSession() reconnect to the same geckodriver after a Firefox crash.
type pooledSession struct {
	wd      selenium.WebDriver
	caps    selenium.Capabilities
	service *selenium.Service
	baseURL string // http://localhost:<port> for this slot's geckodriver
}

// Browser holds a fixed pool of independent geckodriver+Firefox pairs, one per
// concurrent worker.  Geckodriver only supports a single active session per
// process, so each slot runs its own geckodriver on a distinct port
// (4444, 4445, 4446 …).  FillApplication borrows a slot, uses it, and returns
// it when done.
type Browser struct {
	pool chan pooledSession // buffered; size == concurrency
}

// NewBrowser starts concurrency independent geckodriver processes (on ports
// 4444, 4445, …) each managing its own Firefox session, and pools them for
// reuse.  Geckodriver only supports one active session per process, so a
// separate process per slot is required for true parallelism.
// When profileDir is set and concurrency > 1, the base profile is cloned into
// numbered siblings (profile-0, profile-1, …) so each slot has its own
// independent Firefox profile and Simplify stays logged in on every slot.
func NewBrowser(headful bool, profileDir string, concurrency int) (*Browser, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	if profileDir != "" {
		if _, err := os.Stat(filepath.Join(profileDir, "prefs.js")); os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"profile directory %q has not been initialized — run with --setup first",
				profileDir,
			)
		}
	}

	geckodriverPath, err := exec.LookPath("geckodriver")
	if err != nil {
		return nil, fmt.Errorf(
			"geckodriver not found in PATH\n"+
				"Install it for your platform:\n"+
				"  Ubuntu/Debian : sudo apt install firefox-geckodriver\n"+
				"  Fedora/RHEL   : sudo dnf install geckodriver\n"+
				"  Arch Linux    : sudo pacman -S geckodriver\n"+
				"  macOS         : brew install geckodriver\n"+
				"  Manual        : https://github.com/mozilla/geckodriver/releases",
		)
	}

	// Do NOT set dom.webdriver.enabled or useAutomationExtension via Prefs —
	// Firefox 75+ locks dom.webdriver.enabled and geckodriver will refuse to
	// create the session with "Failed to set preferences".  The webdriver flag
	// is already masked at runtime by injectStealthJS (Object.defineProperty).

	b := &Browser{pool: make(chan pooledSession, concurrency)}

	// Start one geckodriver per slot on consecutive ports.  Sessions are created
	// sequentially with a short delay between launches so Firefox has time to
	// initialise before the next process starts.
	const basePort = 4444
	for i := 0; i < concurrency; i++ {
		if i > 0 {
			time.Sleep(1500 * time.Millisecond)
		}

		port := basePort + i
		svc, err := selenium.NewGeckoDriverService(geckodriverPath, port)
		if err != nil {
			b.closeAll()
			return nil, fmt.Errorf("start geckodriver %d/%d on :%d: %w\n(is port %d already in use?)",
				i+1, concurrency, port, err, port)
		}
		baseURL := fmt.Sprintf("http://localhost:%d", port)

		// Resolve the profile directory for this slot.
		slotProfile := profileDir
		if profileDir != "" && concurrency > 1 {
			slotProfile = fmt.Sprintf("%s-%d", profileDir, i)
			if err := prepareProfileClone(profileDir, slotProfile); err != nil {
				_ = svc.Stop()
				b.closeAll()
				return nil, fmt.Errorf("prepare profile clone %d: %w", i, err)
			}
		} else if slotProfile != "" {
			for _, f := range []string{"lock", ".parentlock"} {
				_ = os.Remove(filepath.Join(slotProfile, f))
			}
		}

		caps := buildCaps(headful, slotProfile)
		wd, err := selenium.NewRemote(caps, baseURL)
		if err != nil {
			_ = svc.Stop()
			b.closeAll()
			return nil, fmt.Errorf("open Firefox session %d/%d: %w", i+1, concurrency, err)
		}
		_ = wd.SetPageLoadTimeout(30 * time.Second)
		_ = wd.SetImplicitWaitTimeout(0)
		_ = wd.Get("about:blank")
		b.pool <- pooledSession{wd: wd, caps: caps, service: svc, baseURL: baseURL}
		log.Printf("[browser] session %d/%d ready (port %d)", i+1, concurrency, port)
	}

	return b, nil
}

// buildCaps constructs Firefox WebDriver capabilities for the given headful
// flag and optional profile directory.
func buildCaps(headful bool, profileDir string) selenium.Capabilities {
	var args []string
	if !headful {
		args = append(args, "-headless")
	}
	if profileDir != "" {
		args = append(args, "-profile", profileDir)
	}
	ffCaps := firefox.Capabilities{Args: args}
	caps := selenium.Capabilities{"browserName": "firefox"}
	caps.AddFirefox(ffCaps)
	return caps
}

// prepareProfileClone always rebuilds dst as a fresh copy of src so that any
// changes made to the base profile (re-login, extension updates, preference
// changes) are reflected in every concurrent slot.  The old clone is removed
// first to guarantee the copy is clean.
func prepareProfileClone(src, dst string) error {
	log.Printf("[browser] syncing profile clone → %s", dst)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("remove old clone: %w", err)
	}
	if err := copyDir(src, dst); err != nil {
		return fmt.Errorf("copy profile: %w", err)
	}
	if freed, err := CompactProfile(dst); err == nil && freed > 0 {
		log.Printf("[browser] compacted clone — freed %.1f MB", float64(freed)/1e6)
	}
	for _, f := range []string{"lock", ".parentlock"} {
		_ = os.Remove(filepath.Join(dst, f))
	}
	return nil
}

// copyDir recursively copies src into dst, creating dst when it does not exist.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

// CompactProfile removes cached and ephemeral data from a Firefox profile
// directory that is not needed for automation: HTTP cache, startup cache,
// thumbnails, crash dumps, telemetry, session-restore data, and temporary
// origin storage.  Extension data, cookies, preferences, and saved passwords
// are left untouched so the Simplify extension stays authenticated.
// Returns the total bytes freed.
func CompactProfile(dir string) (int64, error) {
	cacheDirs := []string{
		"cache2",                               // HTTP response cache (largest)
		"cache",                                // Legacy HTTP cache
		"startupCache",                         // Startup cache
		"thumbnails",                           // New-tab-page thumbnails
		"crashes",                              // Crash reporter data
		"datareporting",                        // Telemetry
		"minidumps",                            // Crash minidumps
		"saved-telemetry-pings",                // Telemetry pings
		"sessionstore-backups",                 // Session-restore backups
		filepath.Join("storage", "temporary"),  // Temporary origin storage (not extension data)
	}
	cacheFiles := []string{
		"sessionstore.jsonlz4",
		"sessionCheckpoints.json",
	}

	var freed int64
	du := func(path string) int64 {
		var n int64
		_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				n += info.Size()
			}
			return nil
		})
		return n
	}
	for _, rel := range cacheDirs {
		p := filepath.Join(dir, rel)
		freed += du(p)
		_ = os.RemoveAll(p)
	}
	for _, f := range cacheFiles {
		p := filepath.Join(dir, f)
		if info, err := os.Stat(p); err == nil {
			freed += info.Size()
			_ = os.Remove(p)
		}
	}
	return freed, nil
}

// copyFile copies a single file preserving permissions.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// RunSetup launches Firefox directly (without geckodriver) with profileDir as
// the active profile, navigates to the Simplify extension page, and waits for
// the user to close the window.  Using Firefox directly (rather than via
// geckodriver) avoids the "Failed to set preferences" error that geckodriver
// raises on uninitialised profile directories, and ensures the profile is
// written out cleanly when Firefox exits normally.
func RunSetup(ctx context.Context, profileDir string) error {
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return fmt.Errorf("create profile directory %q: %w", profileDir, err)
	}
	// Remove stale lock files left by any previously killed Firefox process.
	// Firefox refuses to open a profile that has these files even when no
	// other Firefox instance is actually using it.
	for _, f := range []string{"lock", ".parentlock"} {
		_ = os.Remove(filepath.Join(profileDir, f))
	}

	var ffPath string
	for _, name := range []string{"firefox", "firefox-esr", "firefox-bin"} {
		if p, err := exec.LookPath(name); err == nil {
			ffPath = p
			break
		}
	}
	if ffPath == "" {
		return fmt.Errorf("firefox binary not found in PATH (tried: firefox, firefox-esr, firefox-bin)")
	}

	fmt.Println("[setup] Opening Firefox with your persistent profile…")
	fmt.Println("[setup]   1. Install the Simplify extension from the page that opens.")
	fmt.Println("[setup]   2. Click the Simplify icon and log in with your account.")
	fmt.Println("[setup]   3. Configure autofill (name, email, resume, etc.).")
	fmt.Println("[setup]   4. Close Firefox when done — your profile will be saved automatically.")

	cmd := exec.CommandContext(ctx, ffPath,
		"--no-remote",
		"--profile", profileDir,
		"https://addons.mozilla.org/en-US/firefox/addon/simplify-jobs/",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		// Non-zero exit is normal when the user force-closes the window.
		log.Printf("[setup] Firefox exited: %v", err)
	}

	fmt.Printf("[setup] Profile saved to %q.\n", profileDir)
	fmt.Println("[setup] Run 'apply --profile-dir <dir> --simplify-wait 3 ...' to start applying.")
	return nil
}

// Close quits all pooled Firefox sessions and stops every geckodriver process.
// Sessions currently in use by workers are terminated when their geckodriver
// exits.
func (b *Browser) Close() { b.closeAll() }

// closeAll drains the pool, quitting each session and stopping its geckodriver.
// Safe to call during error cleanup in NewBrowser before the Browser is returned.
func (b *Browser) closeAll() {
	for {
		select {
		case s := <-b.pool:
			_ = s.wd.Quit()
			_ = s.service.Stop()
		default:
			return
		}
	}
}

// returnSession navigates the session back to about:blank and returns it to
// the pool.  If the session is dead (Firefox window closed or crashed),
// it reconnects to the same geckodriver process and spawns a replacement in
// the background so the pool stays at its original capacity.
func (b *Browser) returnSession(s pooledSession) {
	if err := s.wd.Get("about:blank"); err == nil {
		_ = s.wd.DeleteAllCookies()
		b.pool <- s
		return
	}
	// Firefox is gone — reconnect to the same geckodriver (still running) and
	// create a fresh session on it.  Do this asynchronously so the worker
	// is not blocked.
	go func() {
		fresh, err := selenium.NewRemote(s.caps, s.baseURL)
		if err != nil {
			log.Printf("[browser] could not replace dead session on %s: %v — pool capacity reduced by 1", s.baseURL, err)
			return
		}
		_ = fresh.SetPageLoadTimeout(30 * time.Second)
		_ = fresh.SetImplicitWaitTimeout(0)
		_ = fresh.Get("about:blank")
		b.pool <- pooledSession{wd: fresh, caps: s.caps, service: s.service, baseURL: s.baseURL}
		log.Printf("[browser] dead session replaced on %s", s.baseURL)
	}()
}

// ErrWindowClosed is returned by FillApplication when the user closes the
// browser tab or window during a headful session.  processOne maps this to
// "skipped" rather than "error" so the URL is not added to the failure list.
var ErrWindowClosed = fmt.Errorf("window closed by user")

// isInstantSkipError reports whether err represents a condition the user cannot
// act on in headful mode (dead job page, auth wall, HTTP error), so the normal
// 10-second headful pause is skipped — saving time during bulk runs.
func isInstantSkipError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"no application form found",
		"sign-in required",
		"http 4", "http 5",
		"job posting unavailable",
		"page redirected to error",
		"workday requires",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// isWindowClosedErr reports whether a WebDriver error indicates the browser
// session or window was closed (by the user or by the browser crashing).
func isWindowClosedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"no such window",
		"no such session",
		"invalid session id",
		"target window already closed",
		"session deleted",
		"window was already closed",
		"browsing context has been discarded",
		"tried to run command without establishing a connection",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// FillApplication opens a fresh Firefox session, navigates to job.URL,
// fills in the applicant details and uploads the resume PDF, then
// optionally clicks Submit.  The session is always closed when done.
func (b *Browser) FillApplication(
	ctx context.Context,
	job *JobInfo,
	info ApplicantInfo,
	resumePath string,
	flags FillFlags,
) (retErr error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Acquire an exclusive session from the pool.  Block until one is
	// available or the context is cancelled.
	var s pooledSession
	select {
	case s = <-b.pool:
	case <-ctx.Done():
		return ctx.Err()
	}
	wd := s.wd

	defer func() {
		if retErr != nil {
			// User closed the tab/window — not a failure, just skip.
			if isWindowClosedErr(retErr) {
				log.Printf("[apply] browser window closed by user — skipping to next URL")
				retErr = ErrWindowClosed
			} else if flags.Hold {
				log.Printf("[hold] error — close the Firefox window to continue: %v", retErr)
				waitForWindowClose(ctx, wd)
				retErr = nil
			} else if flags.Headful && !isInstantSkipError(retErr) {
				// Skip the pause for errors the user cannot act on (dead pages,
				// auth walls, HTTP errors) — only pause when the filled form is
				// worth inspecting.
				log.Printf("[apply] headful error — keeping browser open 10 s: %v", retErr)
				time.Sleep(10 * time.Second)
			} else if flags.Headful {
				log.Printf("[apply] headful skip (auto): %v", retErr)
			}
		}
		// Return the session to the pool if it is still alive; otherwise
		// spawn a replacement in the background so the pool stays full.
		b.returnSession(s)
	}()

	_ = wd.SetPageLoadTimeout(30 * time.Second)
	// Zero implicit wait so FindElements returns immediately for absent fields
	// instead of blocking for several seconds on every miss.
	_ = wd.SetImplicitWaitTimeout(0)

	if err := wd.Get(job.URL); err != nil {
		return fmt.Errorf("navigate to %s: %w", job.URL, err)
	}
	// Give JavaScript-heavy ATS pages time to finish rendering.
	time.Sleep(1200 * time.Millisecond)

	// iCIMS: job-description URLs (ending in /job) need ?mode=apply to load
	// the application form directly.  Navigate there now so the form is ready
	// before cookie / captcha / pre-apply handling runs.
	if job.ATSPlatform == "icims" && !strings.Contains(strings.ToLower(job.URL), "mode=apply") {
		sep := "?"
		if strings.Contains(job.URL, "?") {
			sep = "&"
		}
		if gerr := wd.Get(job.URL + sep + "mode=apply"); gerr == nil {
			time.Sleep(1500 * time.Millisecond)
		}
	}

	// Dismiss any bot-protection challenge before trying to interact with the form.
	// In headful mode this blocks until the human solves it if auto-resolution fails.
	if err := waitForChallenge(wd, flags.Headful); err != nil {
		log.Printf("[captcha] %v — proceeding anyway", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Dismiss any cookie consent banner before interacting with the page.
	// Cookie overlays block clicks on forms and Apply buttons; we accept them
	// unconditionally since we do not need to customise cookie preferences.
	dismissCookieBanner(wd)

	// Many job-description pages show the posting first and only reveal the
	// application form after the visitor clicks "Apply to this Job" or similar.
	// Detect this pattern and click through before attempting to fill.
	clickPreApplyIfNeeded(ctx, wd)

	// European ATS platforms (Jobvite, SmartRecruiters, etc.) gate the application
	// form behind a GDPR/data-processing consent page or modal.  Accept it so the
	// real form renders.  If the wall cannot be dismissed the existing form-ready
	// check below will catch the failure and return an error.
	dismissDataConsentWall(ctx, wd)

	// Verify a real application form is present.  Primary check uses
	// appFormSelector (email / name inputs) because formReadySelector is too
	// broad: a search bar or cookie banner already in the DOM satisfies it even
	// when the actual form modal hasn't rendered yet.
	// Fall back to formReadySelector only for unusual forms without email/name
	// inputs (e.g. resume-only uploads, cover-letter-only pages).
	var formEls []selenium.WebElement
	if formEls, _ = wd.FindElements(selenium.ByCSSSelector, appFormSelector); len(formEls) == 0 {
		waitForElement(ctx, wd, appFormSelector, 8*time.Second)
		formEls, _ = wd.FindElements(selenium.ByCSSSelector, appFormSelector)
	}
	if len(formEls) == 0 {
		// appFormSelector didn't match — last resort: check for any text input
		// so unusual ATS forms (no email/name field) are not incorrectly rejected.
		formEls, _ = wd.FindElements(selenium.ByCSSSelector, formReadySelector)
	}
	if len(formEls) == 0 {
		// Workday: detect the sign-in / create-account wall before falling
		// through to the generic dead-page check.  Workday's auth page has
		// a "Sign In" button and/or a password input with data-automation-id
		// attributes — neither of which matches appFormSelector.
		if job.ATSPlatform == "workday" {
			authEls, _ := wd.FindElements(selenium.ByCSSSelector,
				`[data-automation-id="signIn"], [data-automation-id="createAccount"], [data-automation-id="password"]`)
			if len(authEls) > 0 {
				return fmt.Errorf("workday: account sign-in required — use --profile-dir with saved Workday credentials to auto-apply")
			}
		}

		// Check for email/2FA verification gate before other dead-page checks —
		// these pages have no application form but are recoverable with a cooldown.
		if err := detectEmailVerification(wd); err != nil {
			return err
		}
		// Only run phrase/status detection when there is no form — this avoids
		// false positives on pages that have a "no longer available" notice in
		// a footer or sidebar while still showing a live application form.
		if err := detectDeadPage(wd); err != nil {
			return err
		}
		return fmt.Errorf("no application form found on page (job may be closed or removed)")
	}

	// Trigger the Simplify extension: poll for its injected autofill button,
	// click it once found, then wait for the fill to settle before our
	// supplemental pass runs.
	if flags.SimplifyWait > 0 {
		if waitAndClickSimplify(ctx, wd, flags.SimplifyWait) {
			// Block until the DOM stops changing (Simplify done) rather than
			// sleeping a fixed amount — cap at SimplifyWait so we don't stall
			// indefinitely on broken pages.
			waitForSimplifyDone(ctx, wd, flags.SimplifyWait)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		} else {
			log.Printf("[simplify] autofill button not found — continuing with standard fill")
		}
	}

	// Check for an in-form CAPTCHA before filling — Ashby occasionally shows
	// a Cloudflare Turnstile or hCaptcha widget right on the form page.
	if err := detectInFormCaptcha(wd); err != nil {
		return err
	}

	switch job.ATSPlatform {
	case "greenhouse":
		fillGreenhouse(ctx, wd, info, resumePath)
	case "lever":
		fillLever(ctx, wd, info, resumePath)
	case "ashby":
		fillAshby(ctx, wd, info, resumePath, job)
	case "workable":
		fillWorkable(ctx, wd, info, resumePath)
	case "bamboohr":
		fillBambooHR(ctx, wd, info, resumePath)
	case "personio":
		fillPersonio(ctx, wd, info, resumePath)
	case "workday":
		fillWorkday(ctx, wd, info, resumePath)
	case "icims":
		fillICIMS(ctx, wd, info, resumePath)
	case "jobvite":
		fillGeneric(ctx, wd, info, resumePath)
	default:
		fillGeneric(ctx, wd, info, resumePath)
	}

	// Detect an email verification challenge — Greenhouse sends a one-time code
	// to the applicant's email after recognising the address.  We cannot enter
	// the code programmatically, so abort and let the caller apply a long cooldown.
	if err := detectEmailVerification(wd); err != nil {
		return err
	}

	// Re-check after filling — some platforms inject a CAPTCHA after field
	// interaction (e.g. Ashby Turnstile that only appears on form submission).
	if err := detectInFormCaptcha(wd); err != nil {
		return err
	}

	if flags.Screenshot {
		if data, serr := wd.Screenshot(); serr == nil {
			name := "screenshot_" + safeFilename(job.Company+"_"+job.Title) + ".png"
			_ = os.WriteFile(name, data, 0o644)
		}
	}

	// Hold mode: keep the window open until the user closes it.
	// Takes priority over everything else — no auto-submit.
	if flags.Hold {
		waitForWindowClose(ctx, wd)
		return nil
	}

	if flags.DryRun {
		// In headful dry-run mode keep the window open long enough to inspect.
		if flags.Headful {
			log.Printf("[apply] dry-run: form filled — window stays open for 10 s")
			time.Sleep(10 * time.Second)
		}
		return nil
	}

	// ── Universal required-field mop-up ──────────────────────────────────────
	// Runs after all platform-specific fills to catch any remaining empty
	// required fields — custom employer questions, consent checkboxes, radio
	// groups, etc. — before attempting submission.
	_, mopupLocalPhone := splitPhone(info.Phone)
	if n, err := wd.ExecuteScript(jsFillRequiredFields, []interface{}{
		info.CoverLetter, info.ExpectedSalary, info.Phone, info.Website, mopupLocalPhone,
	}); err == nil {
		if cnt, ok := n.(float64); ok && cnt > 0 {
			log.Printf("[apply] mop-up filled %d remaining required native field(s)", int(cnt))
			time.Sleep(500 * time.Millisecond) // let React re-render after batch fills
		}
	}
	// React Select / combobox mop-up — platform-agnostic
	if res, err := wd.ExecuteScript(jsGetAllUnfilledRequiredComboboxIDs, nil); err == nil {
		if ids, ok := res.([]interface{}); ok && len(ids) > 0 {
			for _, idv := range ids {
				if id, ok := idv.(string); ok && id != "" {
					log.Printf("[apply] mop-up: filling required combobox #%s with first option", id)
					fillGreenhouseComboboxByID(wd, id, "", 800)
				}
			}
		}
	}

	// Ashby-specific submit retry loop: click submit → read the validation
	// error banner → attempt targeted fixes → retry (up to 2 more times).
	// Other platforms use a single submit attempt.
	if job.ATSPlatform == "ashby" {
		return submitWithAshbyRetry(ctx, wd, info, flags)
	}

	if err := clickSubmit(wd); err != nil {
		return err
	}
	// Handle "Submit Application?" confirmation dialogs before waiting.
	// Greenhouse (and some other ATS platforms) show a modal after the
	// first Submit click that requires a second confirmation button click.
	clickConfirmationModal(wd)

	// Post-submit screenshot: capture the page state immediately after the
	// submit+confirm sequence so failures are visible without --headful.
	if flags.Screenshot {
		if data, serr := wd.Screenshot(); serr == nil {
			name := "screenshot_post_" + safeFilename(job.Company+"_"+job.Title) + ".png"
			_ = os.WriteFile(name, data, 0o644)
		}
	}

	return verifySubmission(ctx, wd, flags.Headful || flags.Hold)
}

// waitForWindowClose blocks until the user closes the Firefox window or ctx is
// cancelled.  It polls window handles every 500 ms; when geckodriver reports no
// open windows (or returns an error because the session ended), the function
// returns.
func waitForWindowClose(ctx context.Context, wd selenium.WebDriver) {
	fmt.Println("[hold] Form filled — close the Firefox window to move to the next URL")
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		handles, err := wd.WindowHandles()
		if err != nil || len(handles) == 0 {
			log.Printf("[hold] window closed — proceeding to next URL")
			return
		}
	}
}

// ── Common extras filler ──────────────────────────────────────────────────────

// jsGetUnfilledComboboxIDs returns the id attributes of all React Select
// combobox inputs whose associated hidden required input is still empty (i.e.
// no option has been selected yet).  Used by fillGreenhouse's mop-up pass to
// discover job-specific dropdowns that weren't covered by label-pattern matching.
const jsGetUnfilledComboboxIDs = `
(function(){
    var ids = [];
    var inputs = document.querySelectorAll('input.select__input[role="combobox"]');
    for (var i = 0; i < inputs.length; i++) {
        var inp = inputs[i];
        if (!inp.id) continue;
        // Walk up to the nearest select-shell container and look for the
        // hidden required sentinel that React Select uses for form validation.
        var container = inp.closest('.select-shell');
        if (!container) continue;
        var hidden = container.querySelector('input[tabindex="-1"][aria-hidden="true"]');
        if (!hidden || hidden.value !== '') continue; // not required or already filled
        ids.push(inp.id);
    }
    return ids;
})()
`

// jsGetUnfilledComboboxDetails returns [[id, labelText], ...] for every React
// Select input (input.select__input[role="combobox"]) that still shows a
// placeholder (nothing selected yet).  Unlike jsGetUnfilledComboboxIDs, this
// does NOT require a .select-shell wrapper — it works with the new
// job-boards.greenhouse.io format which uses different container classes.
// The labelText field contains the lowercased text of the nearest <label> so
// the caller can choose an answer (yes/no/first-option) per question.
const jsGetUnfilledComboboxDetails = `
try {
    var _res = [];
    var _seen = {};
    var _inputs = document.querySelectorAll('input.select__input[role="combobox"]');
    for (var _x = 0; _x < _inputs.length; _x++) {
        var _inp = _inputs[_x];
        if (!_inp.id || _seen[_inp.id]) continue;
        var _unfilled = false;
        var _p = _inp.parentElement;
        for (var _i = 0; _i < 10 && _p; _i++) {
            var _hid = _p.querySelector('input[aria-hidden="true"][tabindex="-1"]');
            if (_hid) { _unfilled = (_hid.value === ''); break; }
            var _ph = _p.querySelector('[class*="placeholder"]');
            if (_ph && window.getComputedStyle(_ph).display !== 'none') {
                var _sv = _p.querySelector('[class*="single-value"]');
                if (!_sv) { _unfilled = true; break; }
            }
            _p = _p.parentElement;
        }
        if (!_unfilled) continue;
        _seen[_inp.id] = true;
        var _lbl = '';
        var _c = _inp.parentElement;
        for (var _d = 0; _d < 10 && _c; _d++) {
            var _l = _c.querySelector('label');
            if (_l) { _lbl = _l.textContent.trim().toLowerCase(); break; }
            _c = _c.parentElement;
        }
        _res.push([_inp.id, _lbl]);
    }
    return _res;
} catch(e) { return 'ERR:' + e.toString(); }
`

// jsGetAllUnfilledRequiredComboboxIDs finds React Select / Radix combobox inputs
// that are required but still show no selection, across all ATS platforms.
// Covers the Greenhouse .select-shell pattern, aria-required comboboxes, and
// any platform that uses a hidden required sentinel inside the combobox wrapper.
const jsGetAllUnfilledRequiredComboboxIDs = `
(function(){
    var ids = [];
    function add(id) { if (id && ids.indexOf(id) === -1) ids.push(id); }

    // Greenhouse: .select-shell wrapper with hidden required sentinel
    document.querySelectorAll('.select-shell').forEach(function(shell) {
        var inp = shell.querySelector('input[role="combobox"]');
        var hid = shell.querySelector('input[aria-hidden="true"][tabindex="-1"]');
        if (inp && inp.id && hid && hid.value === '') add(inp.id);
    });

    // Generic: hidden required input whose value is still empty
    document.querySelectorAll('input[required][aria-hidden="true"],input[aria-required="true"][aria-hidden="true"]').forEach(function(hid) {
        if (hid.value !== '') return;
        var p = hid.parentElement;
        for (var i = 0; i < 6 && p; i++) {
            var cb = p.querySelector('[role="combobox"]');
            if (cb && cb.id) { add(cb.id); break; }
            p = p.parentElement;
        }
    });

    // aria-required comboboxes that still have an empty value
    document.querySelectorAll('[role="combobox"][aria-required="true"]').forEach(function(el) {
        if (!el.id || el.value) return;
        // Confirm the container shows a placeholder (nothing selected)
        var p = el.parentElement;
        for (var i = 0; i < 4 && p; i++) {
            if (p.querySelector('[class*="placeholder"]') && !p.querySelector('[class*="single-value"]')) {
                add(el.id); break;
            }
            p = p.parentElement;
        }
    });

    return ids;
})()
`

// jsFillRequiredFields sweeps every visible, still-empty required native field
// (input, textarea, select, radio group, checkbox) and fills it with the best
// synthetic value available.  React Select comboboxes are excluded — they need
// WebDriver click+pick and are handled by the combobox mop-up.
// args: coverLetter, salary, phone, website, localPhone
const jsFillRequiredFields = `
(function(coverLetter, salary, phone, website, localPhone) {
    var nSet = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    var tSet = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
    var filled = 0;

    function fire(el) {
        ['input','change','blur'].forEach(function(ev) {
            el.dispatchEvent(new Event(ev, {bubbles:true}));
        });
    }

    function labelCtx(el) {
        var t = '';
        if (el.id) {
            var l = document.querySelector('label[for="'+el.id+'"]');
            if (l) t = l.textContent;
        }
        if (!t) {
            var p = el.parentElement;
            for (var i = 0; i < 5 && p; i++) {
                var ls = p.querySelectorAll('label');
                if (ls.length) { t = ls[0].textContent; break; }
                p = p.parentElement;
            }
        }
        return (t+' '+(el.name||'')+' '+(el.id||'')+' '+(el.placeholder||'')).toLowerCase();
    }

    var phoneCodeKw = ['country_code','phone_country','dial_code','calling_code',
                       'phone_prefix','country-code','dialcode','countrycode',
                       'phonecode','phone_code','prefix','phoneext','phone_ext',
                       'countrycallingcode','callingcode'];
    function phoneCountrySelectSet() {
        var sels = document.querySelectorAll('select');
        for (var s = 0; s < sels.length; s++) {
            var nm = ((sels[s].name||'')+' '+(sels[s].id||'')).toLowerCase();
            for (var k = 0; k < phoneCodeKw.length; k++) {
                if (nm.indexOf(phoneCodeKw[k]) !== -1 && sels[s].value && sels[s].selectedIndex > 0) return true;
            }
        }
        return false;
    }

    function synthVal(el) {
        var c = labelCtx(el);
        // Skip fields we already fill specifically — returning '' skips the element
        if (/\b(name|email|linkedin)\b/.test(c)) return '';
        if (/phone|mobile|tel/.test(c)) return (localPhone && phoneCountrySelectSet()) ? localPhone : phone;
        if (/salary|compensation|ctc|pay|wage|remunerat/.test(c)) return salary;
        if (/year|yrs\b|experience|how.?long|seniority/.test(c)) return '5';
        if (/notice|start.?date|availab|when.?can.?you|earliest/.test(c)) return '2 weeks';
        if (/why|motivat|interest|excit|passion|tell.?us|about.?your|cover|letter|message|note|comment|additional/.test(c)) return coverLetter;
        if (/url|website|portfolio|github/.test(c)) return website || '';
        if (/city|locat|where.?are.?you|address/.test(c)) return '';   // location handler owns these
        if (el.type === 'number') return '5';
        if (el.type === 'url') return website || '';
        if (el.type === 'date') return '';  // date pickers need specialised handling
        return 'N/A';
    }

    function vis(el) {
        if (el.type === 'hidden') return false;
        var r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
    }

    // 1. Text-like inputs (exclude comboboxes — they're handled separately)
    var inputSel = [
        'input[required]:not([type=hidden]):not([type=checkbox]):not([type=radio]):not([type=file]):not([role=combobox]):not([aria-hidden=true])',
        'input[aria-required=true]:not([type=hidden]):not([type=checkbox]):not([type=radio]):not([type=file]):not([role=combobox]):not([aria-hidden=true])'
    ].join(',');
    Array.from(document.querySelectorAll(inputSel)).forEach(function(el) {
        if (el.disabled || !vis(el) || el.value.trim()) return;
        var v = synthVal(el);
        if (!v) return;
        nSet.call(el, v); fire(el); filled++;
    });

    // 2. Textareas
    Array.from(document.querySelectorAll('textarea[required],textarea[aria-required=true]')).forEach(function(el) {
        if (el.disabled || !vis(el) || el.value.trim()) return;
        tSet.call(el, coverLetter); fire(el); filled++;
    });

    // 3. Native selects
    Array.from(document.querySelectorAll('select[required],select[aria-required=true]')).forEach(function(el) {
        if (el.disabled || !vis(el)) return;
        if (el.value && el.selectedIndex > 0) return;
        for (var i = 1; i < el.options.length; i++) {
            if (el.options[i].value && !el.options[i].disabled) {
                el.selectedIndex = i; fire(el); filled++; break;
            }
        }
    });

    // 4. Radio groups — select first option in any group with nothing checked
    var rg = {};
    Array.from(document.querySelectorAll('input[type=radio][required],input[type=radio][aria-required=true]')).forEach(function(r) {
        var k = r.name || r.id;
        if (!rg[k]) rg[k] = [];
        rg[k].push(r);
    });
    Object.keys(rg).forEach(function(k) {
        var rs = rg[k];
        if (rs.some(function(r){return r.checked;})) return;
        var first = rs.find(function(r){return !r.disabled && vis(r);});
        if (first) { first.click(); fire(first); filled++; }
    });

    // 5. Required unchecked checkboxes (consent / terms)
    Array.from(document.querySelectorAll('input[type=checkbox][required],input[type=checkbox][aria-required=true]')).forEach(function(el) {
        if (!el.disabled && vis(el) && !el.checked) { el.click(); fire(el); filled++; }
    });

    // 6. Blank <select> elements — covers both visible and hidden-overlay selects.
    //    New ATS platforms (e.g. job-boards.greenhouse.io) hide the native <select>
    //    behind a custom UI overlay; the select has non-zero options but fails the
    //    visibility check.  We skip only aria-hidden sentinels and disabled elements.
    Array.from(document.querySelectorAll('select')).forEach(function(el) {
        if (el.disabled) return;
        if (el.getAttribute('aria-hidden') === 'true' || el.tabIndex === -1) return;
        if (el.value || el.selectedIndex > 0) return;
        for (var i = 1; i < el.options.length; i++) {
            if (el.options[i].value && !el.options[i].disabled) {
                el.selectedIndex = i; fire(el); filled++; break;
            }
        }
    });

    return filled;
})(arguments[0], arguments[1], arguments[2], arguments[3])
`

// jsReadAshbyValidationErrors collects the field-level error messages that
// Ashby surfaces in a sticky banner at the top of the form after a failed
// submission attempt.  The banner lists each problematic field by label text
// so we can attempt targeted fixes before the next retry.
// Returns an array of lowercase strings (one per error item), or [] when no
// errors are visible.
const jsReadAshbyValidationErrors = `
(function(){
    var msgs = [];
    // Ashby renders a red summary banner with role="alert" or a class like
    // "_error" / "errorSummary" / "validation-errors".  Each item is in an
    // <li> or a <p> / <span> with the field label.
    var selectors = [
        '[role="alert"] li',
        '[role="alert"] p',
        '[class*="error" i][class*="summar" i] li',
        '[class*="error" i][class*="summar" i] p',
        '[class*="validat" i][class*="error" i] li',
        '[class*="errorList" i] li',
        '[class*="errorList" i] span',
        '[data-qa*="error" i] li',
        '[data-qa*="error" i] span',
        // Fallback: any visible li inside a red/alert container
        '[class*="alert" i] li',
    ];
    var seen = {};
    selectors.forEach(function(sel) {
        try {
            document.querySelectorAll(sel).forEach(function(el) {
                var t = (el.innerText || el.textContent || '').trim().toLowerCase();
                if (t && t.length > 2 && t.length < 300 && !seen[t]) {
                    seen[t] = true;
                    msgs.push(t);
                }
            });
        } catch(e) {}
    });
    // If the selectors above missed it, look for any element with role=alert
    // that has non-trivial text.
    if (msgs.length === 0) {
        document.querySelectorAll('[role="alert"]').forEach(function(el) {
            var t = (el.innerText || '').trim().toLowerCase();
            if (t && t.length > 5 && !seen[t]) {
                seen[t] = true;
                msgs.push(t);
            }
        });
    }
    return msgs;
})()
`

// jsHandleAshbyYesNo clicks the "Yes" or "No" styled button inside a
// Boolean field group whose surrounding label text contains labelNeedle.
// Ashby renders Boolean questions as a pair of <button> elements labelled
// "Yes" and "No" (not radio inputs), so the standard radio/select handlers
// don't reach them.
// arguments[0] = label needle (case-insensitive substring)
// arguments[1] = "yes" or "no"
const jsHandleAshbyYesNo = `
(function(needle, answer) {
    needle = needle.toLowerCase();
    var wantYes = answer.toLowerCase() === 'yes';
    var targetText = wantYes ? 'yes' : 'no';

    // Walk all visible containers that contain the needle in their text.
    var containers = Array.from(document.querySelectorAll(
        '[class*="field" i],[class*="question" i],[class*="form-group" i],[class*="input" i],[data-qa]'
    ));
    for (var i = 0; i < containers.length; i++) {
        var c = containers[i];
        var ct = (c.textContent || '').toLowerCase();
        if (ct.indexOf(needle) === -1) continue;

        // Find button children whose label matches "Yes" or "No" exactly.
        var btns = Array.from(c.querySelectorAll('button'));
        for (var j = 0; j < btns.length; j++) {
            var btn = btns[j];
            var t = (btn.innerText || btn.textContent || '').trim().toLowerCase();
            if (t === targetText) {
                btn.scrollIntoView({block: 'center'});
                btn.click();
                return true;
            }
        }
    }

    // Wider fallback: any button pair labelled Yes/No near the needle.
    var allBtns = Array.from(document.querySelectorAll('button'));
    for (var k = 0; k < allBtns.length; k++) {
        var b = allBtns[k];
        var bt = (b.innerText || b.textContent || '').trim().toLowerCase();
        if (bt !== targetText) continue;
        // Walk up to 6 ancestors to find one that contains the needle.
        var p = b.parentElement;
        for (var d = 0; d < 6 && p; d++) {
            if ((p.textContent || '').toLowerCase().indexOf(needle) !== -1) {
                b.scrollIntoView({block: 'center'});
                b.click();
                return true;
            }
            p = p.parentElement;
        }
    }
    return false;
})(arguments[0], arguments[1])
`

// jsClickFirstMultiValueOption finds the first unchecked checkbox or the first
// clickable option inside a multi-value select widget whose label contains
// needle, and clicks it.  Ashby's "How did you hear about us?" field uses this
// pattern: it is a multi-select backed by checkboxes, not a native <select>.
// arguments[0] = label needle (case-insensitive substring to find the field)
const jsClickFirstMultiValueOption = `
(function(needle) {
    needle = needle.toLowerCase();

    // 1. Try clicking the combobox to open the dropdown first.
    var labels = document.querySelectorAll('label');
    for (var i = 0; i < labels.length; i++) {
        var l = labels[i];
        if (l.textContent.toLowerCase().indexOf(needle) === -1) continue;
        // Try to open the dropdown widget.
        var fid = l.getAttribute('for');
        var trigger = fid ? document.getElementById(fid) : null;
        if (!trigger) trigger = l.querySelector('[role="combobox"], [role="button"], button, input');
        if (!trigger) {
            var sib = l.nextElementSibling;
            while (sib) {
                trigger = sib.querySelector('[role="combobox"], [role="button"], button');
                if (trigger) break;
                if (sib.tagName === 'LABEL') break;
                sib = sib.nextElementSibling;
            }
        }
        if (trigger) { trigger.click(); break; }
    }

    // 2. Wait a tick then click the first available option/checkbox.
    // (Caller sleeps 1 s before running the post-click logic.)
    var opts = document.querySelectorAll('[role="option"],[role="menuitem"],[role="listbox"] [role="option"]');
    for (var j = 0; j < opts.length; j++) {
        var r = opts[j].getBoundingClientRect();
        if (r.width > 0 && r.height > 0) { opts[j].click(); return true; }
    }

    // 3. Fallback: an unchecked checkbox inside the container.
    var allCbs = document.querySelectorAll('input[type="checkbox"]:not(:checked)');
    for (var k = 0; k < allCbs.length; k++) {
        var cb = allCbs[k];
        var p = cb.parentElement;
        for (var d = 0; d < 6 && p; d++) {
            if ((p.textContent || '').toLowerCase().indexOf(needle) !== -1) {
                cb.click(); return true;
            }
            p = p.parentElement;
        }
    }
    return false;
})(arguments[0])
`

// jsSelectRadioContaining finds the radio group whose container text includes
// groupNeedle, then clicks the radio whose label text includes optionNeedle
// (both case-insensitive substrings).  Used for multi-option radio questions
// like "nature of right to work" that have no yes/no answers.
// arguments[0] = group label needle  (e.g. "nature of your right to work")
// arguments[1] = option text needle  (e.g. "unlimited", "citizen", "sponsorship")
const jsSelectRadioContaining = `
(function(groupNeedle, optionNeedle) {
    groupNeedle  = groupNeedle.toLowerCase();
    optionNeedle = optionNeedle.toLowerCase();

    function tryContainer(c) {
        if ((c.textContent || '').toLowerCase().indexOf(groupNeedle) === -1) return false;
        var radios = c.querySelectorAll('input[type="radio"]');
        for (var i = 0; i < radios.length; i++) {
            var r = radios[i];
            if (r.disabled) continue;
            var lbl = document.querySelector('label[for="' + r.id + '"]') || r.closest('label');
            var t = (lbl ? lbl.textContent : (r.getAttribute('aria-label') || r.value || '')).toLowerCase();
            if (t.indexOf(optionNeedle) !== -1) {
                r.scrollIntoView({block: 'center'});
                r.click();
                r.dispatchEvent(new Event('change', {bubbles: true}));
                return true;
            }
        }
        return false;
    }

    var done = false;
    document.querySelectorAll('fieldset').forEach(function(fs) { if (!done) done = tryContainer(fs); });
    if (done) return true;
    var CONT = '[class*="question" i],[class*="field" i],[class*="form-group" i],[class*="group" i],[data-qa]';
    document.querySelectorAll(CONT).forEach(function(d) { if (!done) done = tryContainer(d); });
    return done;
})(arguments[0], arguments[1])
`

// jsForceClickSubmit dispatches a synthetic mouse click on the best submit
// candidate regardless of its disabled state, giving React's onClick handler
// a chance to fire (and display field-level validation errors if anything is
// still missing).  Used as a last-resort after clickSubmit fails.
const jsForceClickSubmit = `
(function(){
    var WORDS = /\b(submit|apply|send|complete|finish|confirm)\b/i;
    var candidates = Array.from(document.querySelectorAll(
        'button[type="submit"], input[type="submit"], button, [role="button"]'
    ));
    for (var i = 0; i < candidates.length; i++) {
        var el = candidates[i];
        var r = el.getBoundingClientRect();
        if (r.width === 0 || r.height === 0) continue;
        var lbl = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
        if (el.type === 'submit' || WORDS.test(lbl)) {
            el.scrollIntoView({block: 'center'});
            el.dispatchEvent(new MouseEvent('click', {bubbles:true, cancelable:true}));
            return true;
        }
    }
    return false;
})()
`

// jsSelectOption picks the best-matching option in the first visible <select>
// that matches the CSS selector.  Tries (1) exact value, (2) exact text,
// (3) prefix text, (4) substring text — all case-insensitive, skipping blank
// placeholder options.
const jsSelectOption = `
(function(){
    var sel=arguments[0], lv=arguments[1].toLowerCase().trim();
    function pick(s){
        var opts=s.options;
        for(var p=0;p<4;p++){
            for(var i=0;i<opts.length;i++){
                if(!opts[i].value) continue;
                var tv=opts[i].value.toLowerCase().trim(), tt=opts[i].text.toLowerCase().trim();
                var match=(p===0&&tv===lv)||(p===1&&tt===lv)||
                          (p===2&&tt.indexOf(lv)===0)||(p===3&&tt.indexOf(lv)!==-1);
                if(match){s.selectedIndex=i;s.dispatchEvent(new Event('change',{bubbles:true}));return true;}
            }
        }
        return false;
    }
    var ss=document.querySelectorAll(sel);
    for(var j=0;j<ss.length;j++){
        var s=ss[j];
        if(s.disabled) continue;
        if(s.offsetParent===null&&window.getComputedStyle(s).position!=='fixed') continue;
        if(pick(s)) return true;
    }
    return false;
})();
`

// jsSelectByLabel picks an option in the <select> whose label text contains
// needle (case-insensitive), using the same 4-level matching as jsSelectOption.
const jsSelectByLabel = `
(function(){
    var needle=arguments[0].toLowerCase(), lv=arguments[1].toLowerCase().trim();
    function pick(s){
        var opts=s.options;
        for(var p=0;p<4;p++){
            for(var i=0;i<opts.length;i++){
                if(!opts[i].value) continue;
                var tv=opts[i].value.toLowerCase().trim(), tt=opts[i].text.toLowerCase().trim();
                var match=(p===0&&tv===lv)||(p===1&&tt===lv)||
                          (p===2&&tt.indexOf(lv)===0)||(p===3&&tt.indexOf(lv)!==-1);
                if(match){s.selectedIndex=i;s.dispatchEvent(new Event('change',{bubbles:true}));return true;}
            }
        }
        return false;
    }
    function findSel(lbl){
        var fid=lbl.getAttribute('for');
        var s=fid?document.getElementById(fid):null;
        if(s&&s.tagName==='SELECT') return s;
        s=lbl.querySelector('select'); if(s) return s;
        var sib=lbl.nextElementSibling;
        while(sib){
            if(sib.tagName==='SELECT') return sib;
            var inner=sib.querySelector('select'); if(inner) return inner;
            if(sib.tagName==='LABEL') break;
            sib=sib.nextElementSibling;
        }
        // Also walk upward one level (form-group pattern)
        if(lbl.parentElement){
            var ps=lbl.parentElement.querySelector('select');
            if(ps) return ps;
        }
        return null;
    }
    var labels=document.querySelectorAll('label');
    for(var i=0;i<labels.length;i++){
        if(labels[i].textContent.toLowerCase().indexOf(needle)===-1) continue;
        var s=findSel(labels[i]);
        if(!s||s.disabled) continue;
        if(s.offsetParent===null&&window.getComputedStyle(s).position!=='fixed') continue;
        if(pick(s)) return true;
    }
    return false;
})();
`

// jsHandleWorkEligibility answers "authorized to work" and "require
// sponsorship" questions — radio buttons, <select> dropdowns, or
// [role="radio"] custom widgets — in a single round-trip.
// arguments[0] = "yes"|"no" for work-auth
// arguments[1] = "yes"|"no" for sponsorship
const jsHandleWorkEligibility = `
(function(authAns, sponsorAns){
    var AUTH=/\b(authorized|authorised|legally\s+(?:able|eligible|authorized|allowed)|right\s+to\s+work|eligible\s+to\s+work|work\s+authoriz|work\s+permit|authorization\s+to\s+work|arbeitserlaubnis|arbeitsgenehmigung|arbeitsberechtigung|recht\s+auf\s+arbeit|rechtlich\s+berechtigt|berechtigt\s+zu\s+arbeiten|befugt\s+zu\s+arbeiten)\b/i;
    var SPONSOR=/\b(sponsorship|visa\s+sponsor|require\s+(?:a\s+)?(?:visa|sponsor)|will\s+you\s+(?:now|in\s+the\s+future)\s+require|need\s+(?:visa|sponsor|h[\-\s]?1b)|aufenthaltserlaubnis|aufenthaltstitel|visa[\-\s]?sponsoring|visumsponsoring|ben[öo]tigen\s+sie\s+(?:ein\s+)?visum|visumsbedarf)\b/i;
    var YES=/^\s*yes\s*$/i, NO=/^\s*no\s*$/i;

    function pickSelect(container, wantYes){
        var sel=container.querySelector('select');
        if(!sel||sel.disabled) return false;
        var re=wantYes?YES:NO;
        for(var i=0;i<sel.options.length;i++){
            if(sel.options[i].value&&re.test(sel.options[i].text)){
                sel.selectedIndex=i; sel.dispatchEvent(new Event('change',{bubbles:true})); return true;
            }
        }
        return false;
    }
    function pickRadio(container, wantYes){
        var radios=container.querySelectorAll('input[type="radio"],[role="radio"]');
        var re=wantYes?YES:NO;
        for(var i=0;i<radios.length;i++){
            var r=radios[i]; if(r.disabled) continue;
            var lbl=document.querySelector('label[for="'+r.id+'"]')||r.closest('label');
            var t=(lbl?lbl.textContent:(r.getAttribute('aria-label')||r.value||'')).trim();
            if(re.test(t)){
                r.scrollIntoView({block:'center'}); r.click();
                r.dispatchEvent(new Event('change',{bubbles:true})); return true;
            }
        }
        return false;
    }
    function handle(container, isAuth){
        var wantYes=isAuth?(authAns==='yes'):(sponsorAns==='yes');
        return pickSelect(container,wantYes)||pickRadio(container,wantYes);
    }

    var authDone=false, sponsorDone=false;

    // 1. Fieldset/legend (most semantic)
    document.querySelectorAll('fieldset').forEach(function(fs){
        var leg=fs.querySelector('legend'); if(!leg) return;
        if(!authDone&&AUTH.test(leg.textContent)) authDone=handle(fs,true);
        if(!sponsorDone&&SPONSOR.test(leg.textContent)) sponsorDone=handle(fs,false);
    });

    // 2. Common form-group/question containers
    if(!authDone||!sponsorDone){
        var divs=document.querySelectorAll(
            '[class*="question"],[class*="field-group"],[class*="form-group"],[class*="form-field"],[class*="input-group"],[class*="radio-group"]'
        );
        divs.forEach(function(d){
            var t=d.textContent;
            if(!authDone&&AUTH.test(t)) authDone=handle(d,true);
            if(!sponsorDone&&SPONSOR.test(t)) sponsorDone=handle(d,false);
        });
    }

    return {auth:authDone,sponsor:sponsorDone};
})(arguments[0],arguments[1]);
`

// jsHandleWorkRestrictions answers questions about NDAs, non-compete clauses,
// cooling-off periods, and other post-employment work restrictions with "no"
// (no such restrictions exist).  Handles radio buttons, native <select>
// dropdowns, and Ashby-style Yes/No button pairs in a single round-trip.
const jsHandleWorkRestrictions = `
(function(){
    var RE = /\b(non[\s-]?disclosure|nda\b|non[\s-]?compete|non[\s-]?competition|non[\s-]?solicitation|cool(?:ing)?[\s-]?(?:off)?[\s-]?period|restrictive\s+covenant|work\s+restriction|confidentiality\s+agreement|post[\s-]?employment\s+restrict|garden\s+leave|restraint\s+of\s+trade)\b/i;
    var NO_RE = /^\s*no\s*$/i;

    function pickNo(container) {
        var radios = container.querySelectorAll('input[type="radio"],[role="radio"]');
        for (var i = 0; i < radios.length; i++) {
            var r = radios[i]; if (r.disabled) continue;
            var lbl = document.querySelector('label[for="'+r.id+'"]') || r.closest('label');
            var t = (lbl ? lbl.textContent : (r.getAttribute('aria-label') || r.value || '')).trim();
            if (NO_RE.test(t)) {
                r.scrollIntoView({block:'center'}); r.click();
                r.dispatchEvent(new Event('change',{bubbles:true})); return true;
            }
        }
        var sel = container.querySelector('select');
        if (sel && !sel.disabled) {
            for (var i = 0; i < sel.options.length; i++) {
                if (sel.options[i].value && NO_RE.test(sel.options[i].text)) {
                    sel.selectedIndex = i; sel.dispatchEvent(new Event('change',{bubbles:true})); return true;
                }
            }
        }
        var btns = container.querySelectorAll('button');
        for (var i = 0; i < btns.length; i++) {
            var t2 = (btns[i].innerText || btns[i].textContent || '').trim().toLowerCase();
            if (t2 === 'no') { btns[i].scrollIntoView({block:'center'}); btns[i].click(); return true; }
        }
        return false;
    }

    document.querySelectorAll('fieldset').forEach(function(fs) {
        var leg = fs.querySelector('legend'); if (!leg) return;
        if (RE.test(leg.textContent)) pickNo(fs);
    });
    document.querySelectorAll('[class*="question"],[class*="field-group"],[class*="form-group"],[class*="form-field"],[class*="radio-group"],[data-qa]').forEach(function(d) {
        if (RE.test(d.textContent)) pickNo(d);
    });
    return true;
})()
`

// jsHandleEEORadios answers gender and ethnicity radio-button groups that
// appear in voluntary self-identification (EEO) sections.  Used on Ashby and
// any other ATS that renders EEO as radio inputs rather than <select> elements.
//
// arguments[0] = gender value  ("male"|"female"|"non-binary"|"decline")
// arguments[1] = ethnicity value ("white"|"black"|"hispanic"|"asian"|
//                "american-indian"|"pacific-islander"|"two-or-more"|"decline")
//
// Each option is scored against the requested value; the highest-scoring
// option wins.  "decline" values explicitly match "prefer not to answer" /
// "decline to self-identify" phrasing and receive a boosted score so they
// are never accidentally chosen when a specific demographic is requested.
const jsHandleEEORadios = `
(function(genderVal, ethnicityVal){
    var GENDER_Q  = /\bgender\b|\bgeschlecht\b/i;
    var ETH_Q     = /\b(ethnicity|ethnic\s+(?:origin|group)|race(?:\s*\/\s*ethnicity)?|racial\s+background|ethnizit[äa]t|ethnische\s+herkunft|volkszugeh[öo]rigkeit)\b/i;
    var DECLINE_T = /prefer\s+not|decline|choose\s+not|do\s+not\s+wish|not\s+to\s+(?:answer|disclose|identify)|not\s+disclose|keine\s+angabe|nicht\s+angeben|lieber\s+nicht|m[öo]chte\s+nicht\s+(?:antworten|angeben)|nicht\s+angegeben|lehne\s+ab|keine\s+antwort/i;

    function pickBest(container, scoreFn) {
        var els = Array.from(container.querySelectorAll('input[type="radio"],[role="radio"]'));
        if (!els.length) return false;
        var best = null, bestScore = -Infinity;
        els.forEach(function(r) {
            if (r.disabled) return;
            var lbl = document.querySelector('label[for="'+r.id+'"]') || r.closest('label');
            var txt = (lbl
                ? lbl.textContent
                : (r.getAttribute('aria-label') || r.value || '')
            ).trim();
            var s = scoreFn(txt);
            if (s > bestScore) { bestScore = s; best = r; }
        });
        if (!best || bestScore < 1) return false;
        best.scrollIntoView({block:'center'});
        best.click();
        best.dispatchEvent(new Event('change',{bubbles:true}));
        return true;
    }

    function scoreGender(txt) {
        var t = txt.toLowerCase();
        if (DECLINE_T.test(t)) return genderVal === 'decline' ? 10 : -10;
        if (genderVal === 'male')       return /^\s*male\b|^\s*man\b/.test(t) ? 8 : 0;
        if (genderVal === 'female')     return /^\s*female\b|^\s*woman\b/.test(t) ? 8 : 0;
        if (genderVal === 'non-binary') return /non[\s\-]?binary|non[\s\-]?conform|genderqueer|gender[\s\-]?fluid/.test(t) ? 8 : 0;
        return 0;
    }

    function scoreEthnicity(txt) {
        var t = txt.toLowerCase();
        if (DECLINE_T.test(t)) return ethnicityVal === 'decline' ? 10 : -10;
        var MAP = {
            'white':            /\bwhite\b/,
            'black':            /\bblack\b|african\s+american|african\s+canadian/,
            'hispanic':         /hispanic|latino/,
            'asian':            /\basian\b/,
            'american-indian':  /american\s+indian|alaska\s+native|native\s+american/,
            'pacific-islander': /pacific\s+islander|hawaiian/,
            'two-or-more':      /two\s+or\s+more|multiracial|multiple\s+race/,
        };
        var re = MAP[ethnicityVal];
        return (re && re.test(t)) ? 8 : 0;
    }

    var genderDone = false, ethDone = false;

    // Strategy 1: fieldset/legend (most semantic HTML)
    document.querySelectorAll('fieldset').forEach(function(fs) {
        var leg = fs.querySelector('legend'); if (!leg) return;
        var lt = leg.textContent;
        if (!genderDone && GENDER_Q.test(lt))  genderDone = pickBest(fs, scoreGender);
        if (!ethDone    && ETH_Q.test(lt))     ethDone    = pickBest(fs, scoreEthnicity);
    });

    // Strategy 2: common question/field containers (covers Ashby React UI and others)
    if (!genderDone || !ethDone) {
        var CONTAINERS = [
            '[class*="question"]', '[class*="field-group"]', '[class*="form-group"]',
            '[class*="eeo"]', '[class*="eeoc"]', '[class*="voluntary"]',
            '[class*="diversity"]', '[class*="demographic"]',
            '[data-qa*="gender"]', '[data-qa*="ethnicity"]', '[data-qa*="race"]',
        ].join(',');
        document.querySelectorAll(CONTAINERS).forEach(function(c) {
            var ct = c.textContent;
            if (!genderDone && GENDER_Q.test(ct)) genderDone = pickBest(c, scoreGender);
            if (!ethDone    && ETH_Q.test(ct))    ethDone    = pickBest(c, scoreEthnicity);
        });
    }

    return {gender: genderDone, ethnicity: ethDone};
})(arguments[0], arguments[1]);
`

// jsHandleOptionalSelects fills EEO dropdowns with "prefer not to answer" /
// "decline" and answers "how did you hear about us" with "Indeed" or "Other".
// All in one round-trip since these fields are typically non-required but
// leaving them blank occasionally blocks submission.
const jsHandleOptionalSelects = `
(function(){
    var EEO=/\b(gender|sex(?:ual)?|race|ethnicity|ethnic\s+(?:origin|group)|national\s+origin|veteran|disability|disabilities|eeoc?|protected\s+class|geschlecht|ethnizit[äa]t|nationalit[äa]t|behinderung)\b/i;
    var HEARD=/\b(how\s+(?:did\s+you|have\s+you)\s+(?:hear|find|learn|discover)|referral\s+source|where\s+did\s+you\s+(?:hear|find)|how\s+did\s+you\s+come\s+across|wie\s+(?:haben\s+sie|sind\s+sie)\s+(?:auf\s+uns|auf\s+diese)|wie\s+sind\s+sie\s+auf\s+die\s+stelle|wo\s+haben\s+sie\s+die\s+stelle|woher\s+(?:kennen|haben)\s+sie)\b/i;
    var DECLINE=['prefer not','decline','not to disclose','do not wish','choose not','not specified','not provided','keine angabe','nicht angeben','lieber nicht'];
    var HEARD_PREF=['indeed','linkedin','job board','job site','online job','internet','other','stellenbörse','jobbörse'];

    document.querySelectorAll('select').forEach(function(sel){
        if(sel.disabled) continue;
        if(sel.offsetParent===null&&window.getComputedStyle(sel).position!=='fixed') return;

        // Collect context text from label + surrounding container
        var ctx='';
        var lbl=document.querySelector('label[for="'+sel.id+'"]');
        if(lbl) ctx+=lbl.textContent;
        var p=sel.parentElement;
        for(var i=0;i<3&&p;i++){ ctx+=p.textContent; p=p.parentElement; }
        ctx=(ctx+' '+(sel.name||'')+' '+(sel.id||'')).toLowerCase();

        if(EEO.test(ctx)){
            for(var di=0;di<DECLINE.length;di++){
                for(var oi=0;oi<sel.options.length;oi++){
                    if(sel.options[oi].value&&sel.options[oi].text.toLowerCase().indexOf(DECLINE[di])!==-1){
                        sel.selectedIndex=oi; sel.dispatchEvent(new Event('change',{bubbles:true})); break;
                    }
                }
                if(sel.selectedIndex>0) break;
            }
        } else if(HEARD.test(ctx)){
            for(var hi=0;hi<HEARD_PREF.length;hi++){
                var found=false;
                for(var oi=0;oi<sel.options.length;oi++){
                    if(sel.options[oi].value&&sel.options[oi].text.toLowerCase().indexOf(HEARD_PREF[hi])!==-1){
                        sel.selectedIndex=oi; sel.dispatchEvent(new Event('change',{bubbles:true})); found=true; break;
                    }
                }
                if(found) break;
            }
        }
    });
    return true;
})();
`

// jsAcceptPrivacyConsent ticks any unchecked checkbox whose label mentions
// privacy, data processing, GDPR / DSGVO, application consent, or is a bare
// "I agree" / "I accept" acknowledgement.  Personio and several other ATS
// platforms make this checkbox mandatory; leaving it unticked prevents the
// Submit button from becoming active.  Ashby uses "I agree" with no further
// wording.
const jsAcceptPrivacyConsent = `
(function(){
    var CONSENT = /\b(privacy|data\s*(?:processing|protection|policy)|gdpr|dsgvo|datenschutz|personal\s+data|application\s+terms|consent\s+to\s+(?:the\s*)?(?:processing|collection)|i\s+agree|i\s+accept|ich\s+stimme\s+zu|ich\s+akzeptiere)\b/i;
    var cbs = document.querySelectorAll('input[type="checkbox"]:not(:checked)');
    for (var i = 0; i < cbs.length; i++) {
        var cb = cbs[i];
        if (cb.disabled) continue;
        var lbl = document.querySelector('label[for="'+cb.id+'"]') || cb.closest('label');
        var txt = (lbl ? lbl.textContent : (cb.getAttribute('aria-label') || cb.name || '')).toLowerCase();
        if (CONSENT.test(txt)) { cb.click(); }
    }
    return true;
})();
`

// jsHandleSalary fills expected-salary/compensation inputs and selects.
// Searches by label text and form-group containers so React/Vue forms are covered.
// When val is "Negotiable" (the baked-in default) and the field is a <select>,
// pickSelFallback picks the first non-trivial option so the field is never left blank.
// arguments[0] = salary string (e.g. "85000", "80k-100k", "Negotiable")
const jsHandleSalary = `
(function(val){
    var Q=/\b(salary|compensation|expected\s+pay|desired\s+pay|expected\s+salary|desired\s+salary|salary\s+expect|pay\s+expect|ctc|annual\s+(?:salary|pay|comp)|remuneration|pay\s+range|expected\s+package|gehalt|gehaltsvorstellung|gew[üu]nschtes\s+gehalt|verg[üu]tung|jahresgehalt|bruttogehalt)\b/i;
    var SKIP=/\b(prefer\s+not|not\s+to\s+say|not\s+specified|decline|n\/a)\b/i;
    function fill(el,v){
        if(el.disabled) return false;
        if(el.offsetParent===null&&window.getComputedStyle(el).position!=='fixed') return false;
        el.focus(); el.scrollIntoView({block:'center'});
        try{ var proto=el.tagName==='TEXTAREA'?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;
             Object.getOwnPropertyDescriptor(proto,'value').set.call(el,v); }
        catch(e){ el.value=v; }
        ['input','change','blur'].forEach(function(ev){ el.dispatchEvent(new Event(ev,{bubbles:true})); });
        return true;
    }
    function pickSel(sel,v){
        var lv=v.toLowerCase(),opts=sel.options;
        // 4-pass exact→text→prefix→substring match
        for(var p=0;p<4;p++) for(var i=0;i<opts.length;i++){
            if(!opts[i].value) continue;
            var tv=opts[i].value.toLowerCase().trim(),tt=opts[i].text.toLowerCase().trim();
            if((p===0&&tv===lv)||(p===1&&tt===lv)||(p===2&&tt.indexOf(lv)===0)||(p===3&&tt.indexOf(lv)!==-1)){
                sel.selectedIndex=i; sel.dispatchEvent(new Event('change',{bubbles:true})); return true;
            }
        }
        return false;
    }
    function pickSelFallback(sel){
        // Called when the value didn't match any option (e.g. "Negotiable" on a
        // range-based select).  Pick the middle non-trivial option so the field
        // is not left at the blank placeholder.
        var real=[];
        for(var i=0;i<sel.options.length;i++){
            var o=sel.options[i];
            if(!o.value||SKIP.test(o.text)) continue;
            real.push(i);
        }
        if(!real.length) return false;
        var mid=real[Math.floor(real.length/2)];
        sel.selectedIndex=mid; sel.dispatchEvent(new Event('change',{bubbles:true})); return true;
    }
    function tryContainer(c){
        if(!Q.test(c.textContent)) return false;
        var inp=c.querySelector('input:not([type=hidden]):not([type=file]):not([type=checkbox]):not([type=radio]),textarea');
        if(inp) return fill(inp,val);
        var s=c.querySelector('select');
        if(s&&!s.disabled) return pickSel(s,val)||pickSelFallback(s);
        return false;
    }
    var done=false;
    document.querySelectorAll('fieldset').forEach(function(fs){
        if(done) return;
        var leg=fs.querySelector('legend'); if(leg&&Q.test(leg.textContent)) done=tryContainer(fs);
    });
    if(done) return true;
    document.querySelectorAll('[class*="field"],[class*="group"],[class*="question"],[class*="input-wrap"]').forEach(function(d){
        if(done) return; done=tryContainer(d);
    });
    return done;
})(arguments[0]);
`

// jsHandleAvailability fills notice-period selects/text-inputs and start-date
// date-pickers using label-text and form-group container heuristics.
// arguments[0] = notice period text (e.g. "2 weeks")
// arguments[1] = ISO start date (e.g. "2026-05-26")
const jsHandleAvailability = `
(function(notice, dateStr){
    var Q=/\b(notice\s*period|notice|availability|available\s*(?:from|date|to\s+start)?|start\s*date|earliest\s*start|when\s+can\s+you\s+start|when\s+(?:are\s+you|would\s+you\s+be)\s+available|joining\s+date|commencement|start\s+of\s+(?:work|employment)|k[üu]ndigungsfrist|fr[üu]hestm[öo]glich(?:er)?|verf[üu]gbarkeit|eintrittsdatum|startdatum|wann\s+(?:k[öo]nnen\s+sie|stehen\s+sie)|eintrittstermin|beginn\s+der\s+t[äa]tigkeit)\b/i;
    function fill(el,v){
        if(el.disabled) return false;
        if(el.offsetParent===null&&window.getComputedStyle(el).position!=='fixed') return false;
        el.focus(); el.scrollIntoView({block:'center'});
        try{ Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(el,v); }
        catch(e){ el.value=v; }
        ['input','change','blur'].forEach(function(ev){ el.dispatchEvent(new Event(ev,{bubbles:true})); });
        return true;
    }
    function pickSel(sel,v){
        var lv=v.toLowerCase(),opts=sel.options;
        for(var p=0;p<4;p++) for(var i=0;i<opts.length;i++){
            if(!opts[i].value) continue;
            var tv=opts[i].value.toLowerCase().trim(),tt=opts[i].text.toLowerCase().trim();
            if((p===0&&tv===lv)||(p===1&&tt===lv)||(p===2&&tt.indexOf(lv)===0)||(p===3&&tt.indexOf(lv)!==-1)){
                sel.selectedIndex=i; sel.dispatchEvent(new Event('change',{bubbles:true})); return true;
            }
        }
        return false;
    }
    function handleContainer(c){
        if(!Q.test(c.textContent)) return;
        var sel=c.querySelector('select');
        if(sel&&!sel.disabled&&notice){ pickSel(sel,notice); return; }
        var dateInp=c.querySelector('input[type="date"]');
        if(dateInp&&dateStr){ fill(dateInp,dateStr); return; }
        var inp=c.querySelector('input:not([type=hidden]):not([type=file]):not([type=checkbox]):not([type=radio])');
        if(inp&&notice) fill(inp,notice);
    }
    document.querySelectorAll('fieldset').forEach(function(fs){
        var leg=fs.querySelector('legend'); if(leg&&Q.test(leg.textContent)) handleContainer(fs);
    });
    document.querySelectorAll('[class*="field"],[class*="group"],[class*="question"]').forEach(handleContainer);
    // Direct attribute fallback for date inputs with start/available names
    document.querySelectorAll('input[type="date"]').forEach(function(inp){
        if(inp.disabled) return;
        var nm=(inp.name||inp.id||inp.placeholder||'').toLowerCase();
        if(dateStr&&/start|available|notice|join|commenc/.test(nm)) fill(inp,dateStr);
    });
    return true;
})(arguments[0],arguments[1]);
`

// usStateNames maps 2-letter codes to full names so both representations can
// be tried when filling state text-inputs or <select> dropdowns.
var usStateNames = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas",
	"CA": "California", "CO": "Colorado", "CT": "Connecticut", "DE": "Delaware",
	"FL": "Florida", "GA": "Georgia", "HI": "Hawaii", "ID": "Idaho",
	"IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas",
	"KY": "Kentucky", "LA": "Louisiana", "ME": "Maine", "MD": "Maryland",
	"MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi",
	"MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada",
	"NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico", "NY": "New York",
	"NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma",
	"OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina",
	"SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah",
	"VT": "Vermont", "VA": "Virginia", "WA": "Washington", "WV": "West Virginia",
	"WI": "Wisconsin", "WY": "Wyoming", "DC": "District of Columbia",
	// Canadian provinces
	"AB": "Alberta", "BC": "British Columbia", "MB": "Manitoba",
	"NB": "New Brunswick", "NL": "Newfoundland and Labrador",
	"NS": "Nova Scotia", "ON": "Ontario", "PE": "Prince Edward Island",
	"QC": "Quebec", "SK": "Saskatchewan",
}

// trySetSelect uses jsSelectOption to pick a matching option in a <select>.
func trySetSelect(wd selenium.WebDriver, selector, value string) bool {
	if value == "" {
		return false
	}
	res, err := wd.ExecuteScript(jsSelectOption, []interface{}{selector, value})
	return err == nil && res == true
}

// trySetSelectByLabel uses jsSelectByLabel to pick a matching option in a
// <select> found via its label text.
func trySetSelectByLabel(wd selenium.WebDriver, labelText, optionValue string) bool {
	if optionValue == "" {
		return false
	}
	res, err := wd.ExecuteScript(jsSelectByLabel, []interface{}{labelText, optionValue})
	return err == nil && res == true
}

// jsTypeCombobox finds the text input associated with a label containing
// needle and types val into it, triggering the React synthetic event chain.
// Used for custom combobox/react-select dropdowns (e.g. Ashby location).
// Returns a truthy string when an element was found and typed into.
const jsTypeCombobox = `
var needle=arguments[0].toLowerCase(), val=arguments[1];
var labels=document.querySelectorAll('label');
for(var i=0;i<labels.length;i++){
    var l=labels[i];
    if(l.textContent.trim().toLowerCase().indexOf(needle)===-1) continue;
    var inp=null;
    var fid=l.getAttribute('for');
    if(fid) inp=document.getElementById(fid);
    if(!inp) inp=l.querySelector('input:not([type="hidden"])');
    if(!inp){var p=l.parentElement;if(p) inp=p.querySelector('input:not([type="hidden"])');}
    if(!inp){
        var sib=l.nextElementSibling;
        while(sib){
            if(sib.tagName==='INPUT'&&sib.type!=='hidden'){inp=sib;break;}
            var inner=sib.querySelector('input:not([type="hidden"])');
            if(inner){inp=inner;break;}
            sib=sib.nextElementSibling;
        }
    }
    if(!inp||inp.disabled) continue;
    inp.click(); inp.focus();
    var setter=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set;
    setter.call(inp,val);
    inp.dispatchEvent(new Event('input',{bubbles:true}));
    inp.dispatchEvent(new Event('change',{bubbles:true}));
    return 'ok';
}
return null;
`

// fillComboboxByLabel types value into a custom combobox whose label contains
// labelText, then waits for the listbox to open and clicks the first option
// that contains value.  This handles React-Select / Ashby-style dropdowns
// where there is no native <select> element.
func fillComboboxByLabel(wd selenium.WebDriver, labelText, value string) bool {
	if value == "" {
		return false
	}
	res, err := wd.ExecuteScript(jsTypeCombobox, []interface{}{labelText, value})
	if err != nil || res == nil {
		return false
	}
	time.Sleep(800 * time.Millisecond) // wait for listbox to render
	for _, sel := range []string{
		`[role="listbox"] [role="option"]`,
		`[role="option"]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		valLow := strings.ToLower(value)
		for _, el := range els {
			text, _ := el.Text()
			if strings.Contains(strings.ToLower(text), valLow) {
				_ = el.Click()
				time.Sleep(200 * time.Millisecond)
				return true
			}
		}
		// No text match — click the first visible option rather than leaving
		// the dropdown open with nothing selected.
		if err := els[0].Click(); err == nil {
			time.Sleep(200 * time.Millisecond)
			return true
		}
	}
	// No listbox appeared at all — close any open dropdown so it does not
	// intercept clicks meant for the next field.
	if inps, e := wd.FindElements(selenium.ByCSSSelector, `input[role="combobox"][aria-expanded="true"]`); e == nil {
		for _, inp := range inps {
			inp.SendKeys(selenium.EscapeKey) //nolint:errcheck
		}
	}
	return false
}

// fillSalutation selects "Mr" / "Herr" in salutation/title fields that appear
// on European ATS forms (onlyfy, Personio, etc.).  It tries native <select>
// elements first (by name/id), then label-based select lookup, then radio
// buttons, then plain text inputs.
func fillSalutation(wd selenium.WebDriver) {
	const mr = "Mr"
	const herr = "Herr"

	// Native <select> by name/id pattern (salut, anrede, honorific).
	// "title" is intentionally excluded here — too broad; see label-based pass.
	for _, sel := range []string{
		`select[name*="salut" i]`, `select[id*="salut" i]`,
		`select[name*="anrede" i]`, `select[id*="anrede" i]`,
		`select[name*="honorific" i]`, `select[id*="honorific" i]`,
	} {
		if trySetSelect(wd, sel, mr) || trySetSelect(wd, sel, herr) {
			return
		}
	}

	// Label-based <select> (covers cases where name/id is generic).
	for _, lbl := range []string{"salutation", "anrede", "honorific", "salut"} {
		if trySetSelectByLabel(wd, lbl, mr) || trySetSelectByLabel(wd, lbl, herr) {
			return
		}
	}

	// Radio buttons — click the first option whose visible text is "Mr" or "Herr".
	res, err := wd.ExecuteScript(`
try {
    var radios = document.querySelectorAll('input[type="radio"]');
    for (var i = 0; i < radios.length; i++) {
        var r = radios[i];
        var nm = (r.name || r.id || '').toLowerCase();
        if (!/salut|anrede|honorific|gender_title/.test(nm)) {
            var lbl = document.querySelector('label[for="'+r.id+'"]');
            if (!lbl) continue;
            var txt = lbl.textContent.trim().toLowerCase();
            if (!/salut|anrede|honorific/.test(txt)) continue;
        }
        var lbl2 = document.querySelector('label[for="'+r.id+'"]');
        var val = (r.value || (lbl2 && lbl2.textContent) || '').trim().toLowerCase();
        if (val === 'mr' || val === 'herr' || val === 'mr.' || val === 'mister') {
            r.click();
            return true;
        }
    }
    return false;
} catch(e) { return false; }
`, nil)
	if ok, _ := res.(bool); ok {
		return
	}
	_ = err

	// Text input fallback (rare, but some forms use a free-text title field).
	for _, sel := range []string{
		`input[name*="salut" i]`, `input[id*="salut" i]`,
		`input[name*="anrede" i]`, `input[id*="anrede" i]`,
	} {
		if trySetInput(wd, sel, mr) {
			return
		}
	}
	tryFillByLabel(wd, "salutation", mr)
	tryFillByLabel(wd, "anrede", herr)
}

// fillCommonExtras fills the form fields that are common across all ATS
// platforms but are not covered by the ATS-specific fillers: address,
// professional links, cover letter, work authorization, EEO, and "how did
// you hear".  It is called after every ATS-specific filler.
func fillCommonExtras(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo) {
	// ── Salutation / title ────────────────────────────────────────────────────
	// Many European ATS forms (onlyfy, etc.) require a salutation.  We always
	// answer "Mr" / "Herr" (German).  "title" is kept narrow (must also match
	// "salut" or "anrede" siblings) to avoid clobbering job-title fields.
	fillSalutation(wd)

	// ── City ─────────────────────────────────────────────────────────────────
	if info.City != "" {
		for _, sel := range []string{
			`input[name*="city" i]`, `input[id*="city" i]`,
			`input[placeholder*="city" i]`, `input[autocomplete="address-level2"]`,
		} {
			if trySetInput(wd, sel, info.City) {
				break
			}
		}
		tryFillByLabel(wd, "city", info.City)
		tryFillByLabel(wd, "ort", info.City)
		tryFillByLabel(wd, "wohnort", info.City)
		tryFillByLabel(wd, "stadt", info.City)
	}

	// ── State / Province ──────────────────────────────────────────────────────
	if info.State != "" {
		expanded := usStateNames[strings.ToUpper(info.State)] // full name, may be ""
		stateSelectors := `select[name*="state" i],select[id*="state" i],select[name*="province" i],select[id*="province" i]`
		inputSelectors := `input[name*="state" i],input[id*="state" i],input[name*="province" i],input[autocomplete="address-level1"]`

		// Try the code first, then the full name, for both select and input.
		if !trySetSelect(wd, stateSelectors, info.State) && expanded != "" {
			trySetSelect(wd, stateSelectors, expanded)
		}
		if !trySetSelectByLabel(wd, "state", info.State) && expanded != "" {
			trySetSelectByLabel(wd, "state", expanded)
		}
		trySetSelectByLabel(wd, "province", info.State)
		if expanded != "" {
			trySetSelectByLabel(wd, "province", expanded)
		}
		if !trySetInput(wd, inputSelectors, info.State) {
			tryFillByLabel(wd, "state", info.State)
			tryFillByLabel(wd, "province", info.State)
			tryFillByLabel(wd, "bundesland", info.State)
		}
		trySetSelectByLabel(wd, "bundesland", info.State)
	}

	// ── ZIP / Postal code ─────────────────────────────────────────────────────
	if info.ZipCode != "" {
		for _, sel := range []string{
			`input[name*="zip" i]`, `input[id*="zip" i]`,
			`input[name*="postal" i]`, `input[id*="postal" i]`,
			`input[autocomplete="postal-code"]`,
		} {
			if trySetInput(wd, sel, info.ZipCode) {
				break
			}
		}
		tryFillByLabel(wd, "zip", info.ZipCode)
		tryFillByLabel(wd, "postal", info.ZipCode)
		tryFillByLabel(wd, "postleitzahl", info.ZipCode)
		tryFillByLabel(wd, "plz", info.ZipCode)
	}

	// ── Country ───────────────────────────────────────────────────────────────
	if info.Country != "" {
		countrySelectors := `select[name*="country" i],select[id*="country" i],select[autocomplete="country"],select[autocomplete="country-name"]`
		if !trySetSelect(wd, countrySelectors, info.Country) {
			trySetSelectByLabel(wd, "country", info.Country)
		}
		// Exclude role="combobox" inputs: setting their value via JS fires the
		// React input event which opens the React Select dropdown without then
		// clicking an option, leaving the dropdown open and the field empty.
		// Those are handled below by fillComboboxByLabel.
		for _, sel := range []string{
			`input[name*="country" i]:not([role="combobox"])`,
			`input[id*="country" i]:not([role="combobox"])`,
			`input[autocomplete="country-name"]`,
		} {
			if trySetInput(wd, sel, info.Country) {
				break
			}
		}
		tryFillByLabel(wd, "country", info.Country)
		trySetSelectByLabel(wd, "land", info.Country)
		tryFillByLabel(wd, "land", info.Country)
		trySetSelectByLabel(wd, "heimatland", info.Country)
		// NOTE: React Select combobox fills (location, country) are intentionally
		// omitted here.  Greenhouse handles them in fillGreenhouse before calling
		// fillCommonExtras; Ashby handles them in fillAshby.  Adding generic
		// combobox fills here causes double-fills that visibly re-open dropdowns.
	}

	// ── Website / portfolio ───────────────────────────────────────────────────
	if info.Website != "" {
		for _, sel := range []string{
			`input[name*="website" i]`, `input[id*="website" i]`,
			`input[name*="portfolio" i]`, `input[id*="portfolio" i]`,
			`input[placeholder*="website" i]`, `input[placeholder*="portfolio" i]`,
		} {
			if trySetInput(wd, sel, info.Website) {
				break
			}
		}
		tryFillByLabel(wd, "website", info.Website)
		tryFillByLabel(wd, "portfolio", info.Website)
		tryFillByLabel(wd, "personal site", info.Website)
		tryFillByLabel(wd, "webseite", info.Website)
		tryFillByLabel(wd, "internetseite", info.Website)
	}

	// ── GitHub ────────────────────────────────────────────────────────────────
	if info.GitHubURL != "" {
		for _, sel := range []string{
			`input[name*="github" i]`, `input[id*="github" i]`,
			`input[placeholder*="github" i]`, `input[aria-label*="github" i]`,
		} {
			if trySetInput(wd, sel, info.GitHubURL) {
				break
			}
		}
		tryFillByLabel(wd, "github", info.GitHubURL)
	}

	// ── Cover letter ──────────────────────────────────────────────────────────
	if info.CoverLetter != "" {
		for _, sel := range []string{
			`textarea[name*="cover" i]`, `textarea[id*="cover" i]`,
			`textarea[placeholder*="cover letter" i]`, `textarea[aria-label*="cover letter" i]`,
		} {
			if trySetInput(wd, sel, info.CoverLetter) {
				break
			}
		}
		tryFillByLabel(wd, "cover letter", info.CoverLetter)
		tryFillByLabel(wd, "motivation", info.CoverLetter)
		tryFillByLabel(wd, "anschreiben", info.CoverLetter)
		tryFillByLabel(wd, "motivationsschreiben", info.CoverLetter)
		tryFillByLabel(wd, "bewerbungsschreiben", info.CoverLetter)
	}

	// ── Work authorization & sponsorship ──────────────────────────────────────
	// Always answer — required on Ashby and many other platforms, leaving it
	// blank keeps the Submit button disabled even when every other field is set.
	authAns := info.WorkAuthorized
	if authAns == "" {
		authAns = "yes"
	}
	sponsorAns := info.RequireSponsorship
	if sponsorAns == "" {
		sponsorAns = "no"
	}
	wd.ExecuteScript(jsHandleWorkEligibility, []interface{}{authAns, sponsorAns}) //nolint:errcheck

	// ── NDA / non-compete / cooling-off / work restrictions — always "no" ────
	wd.ExecuteScript(jsHandleWorkRestrictions, nil) //nolint:errcheck

	// ── EEO radio buttons (gender + ethnicity — Ashby and others) ────────────
	genderVal := info.Gender
	if genderVal == "" {
		genderVal = "decline"
	}
	ethnicityVal := info.Ethnicity
	if ethnicityVal == "" {
		ethnicityVal = "decline"
	}
	wd.ExecuteScript(jsHandleEEORadios, []interface{}{genderVal, ethnicityVal}) //nolint:errcheck

	// ── EEO <select> dropdowns + "how did you hear" ───────────────────────────
	wd.ExecuteScript(jsHandleOptionalSelects, nil) //nolint:errcheck

	// ── Privacy / GDPR consent checkbox ──────────────────────────────────────
	// Personio and several other ATS platforms require this checkbox before
	// the Submit button becomes active.
	wd.ExecuteScript(jsAcceptPrivacyConsent, nil) //nolint:errcheck

	// ── Expected salary ───────────────────────────────────────────────────────
	// Always set: defaults to "Negotiable" when the caller provides no value.
	for _, sel := range []string{
		`input[name*="salary" i]`, `input[id*="salary" i]`,
		`input[placeholder*="salary" i]`,
		`input[name*="compensation" i]`, `input[id*="compensation" i]`,
		`input[name*="ctc" i]`,
	} {
		if trySetInput(wd, sel, info.ExpectedSalary) {
			break
		}
	}
	tryFillByLabel(wd, "expected salary", info.ExpectedSalary)
	tryFillByLabel(wd, "desired salary", info.ExpectedSalary)
	tryFillByLabel(wd, "salary expectation", info.ExpectedSalary)
	tryFillByLabel(wd, "annual salary", info.ExpectedSalary)
	tryFillByLabel(wd, "expected compensation", info.ExpectedSalary)
	tryFillByLabel(wd, "compensation", info.ExpectedSalary)
	tryFillByLabel(wd, "expected ctc", info.ExpectedSalary)
	tryFillByLabel(wd, "salary", info.ExpectedSalary)
	trySetSelectByLabel(wd, "expected salary", info.ExpectedSalary)
	trySetSelectByLabel(wd, "salary range", info.ExpectedSalary)
	trySetSelectByLabel(wd, "salary", info.ExpectedSalary)
	tryFillByLabel(wd, "gehaltsvorstellung", info.ExpectedSalary)
	tryFillByLabel(wd, "gewünschtes gehalt", info.ExpectedSalary)
	tryFillByLabel(wd, "gehalt", info.ExpectedSalary)
	tryFillByLabel(wd, "vergütung", info.ExpectedSalary)
	trySetSelectByLabel(wd, "gehaltsvorstellung", info.ExpectedSalary)
	wd.ExecuteScript(jsHandleSalary, []interface{}{info.ExpectedSalary}) //nolint:errcheck

	// ── Notice period / earliest start date ──────────────────────────────────
	np := info.NoticePeriod
	sd := info.EarliestStartDate
	if np != "" || sd != "" {
		// Date-picker inputs (name/id must signal availability or start context)
		if sd != "" {
			for _, sel := range []string{
				`input[type="date"][name*="start" i]`,
				`input[type="date"][id*="start" i]`,
				`input[type="date"][name*="available" i]`,
				`input[type="date"][id*="available" i]`,
				`input[type="date"][name*="notice" i]`,
			} {
				if trySetInput(wd, sel, sd) {
					break
				}
			}
		}
		// Text inputs and selects
		if np != "" {
			for _, sel := range []string{
				`input[name*="notice" i]`, `input[id*="notice" i]`,
				`input[placeholder*="notice period" i]`,
			} {
				if trySetInput(wd, sel, np) {
					break
				}
			}
			for _, sel := range []string{
				`select[name*="notice" i]`, `select[id*="notice" i]`,
			} {
				if trySetSelect(wd, sel, np) {
					break
				}
			}
			tryFillByLabel(wd, "notice period", np)
			tryFillByLabel(wd, "notice", np)
			tryFillByLabel(wd, "earliest start date", np)
			tryFillByLabel(wd, "start date", np)
			tryFillByLabel(wd, "available from", np)
			tryFillByLabel(wd, "availability", np)
			tryFillByLabel(wd, "when can you start", np)
			trySetSelectByLabel(wd, "notice period", np)
			trySetSelectByLabel(wd, "notice", np)
			trySetSelectByLabel(wd, "availability", np)
			trySetSelectByLabel(wd, "start date", np)
			tryFillByLabel(wd, "kündigungsfrist", np)
			tryFillByLabel(wd, "verfügbarkeit", np)
			tryFillByLabel(wd, "eintrittsdatum", np)
			tryFillByLabel(wd, "frühestmöglicher eintrittstermin", np)
			trySetSelectByLabel(wd, "kündigungsfrist", np)
			trySetSelectByLabel(wd, "verfügbarkeit", np)
		}
		wd.ExecuteScript(jsHandleAvailability, []interface{}{np, sd}) //nolint:errcheck
	}
}

// ── ATS-specific form fillers ─────────────────────────────────────────────────

// dialCodeToISO maps international dial prefixes (longest first) to ISO 3166-1
// alpha-2 codes.  Used to infer the Greenhouse phone-country-select value from
// an E.164-formatted phone number (e.g. "+972…" → "IL").
var dialCodeToISO = map[string]string{
	// 3-digit codes
	"+212": "MA", "+213": "DZ", "+216": "TN", "+218": "LY",
	"+220": "GM", "+221": "SN", "+234": "NG", "+254": "KE",
	"+255": "TZ", "+256": "UG", "+260": "ZM", "+263": "ZW",
	"+351": "PT", "+352": "LU", "+353": "IE", "+354": "IS",
	"+356": "MT", "+358": "FI", "+359": "BG",
	"+370": "LT", "+371": "LV", "+372": "EE", "+374": "AM",
	"+375": "BY", "+380": "UA", "+381": "RS", "+382": "ME",
	"+385": "HR", "+386": "SI", "+387": "BA", "+389": "MK",
	"+420": "CZ", "+421": "SK",
	"+852": "HK", "+853": "MO", "+855": "KH", "+856": "LA",
	"+880": "BD", "+886": "TW",
	"+961": "LB", "+962": "JO", "+963": "SY", "+964": "IQ",
	"+965": "KW", "+966": "SA", "+967": "YE", "+968": "OM",
	"+971": "AE", "+972": "IL", "+973": "BH", "+974": "QA",
	"+977": "NP",
	// 2-digit codes
	"+20": "EG", "+27": "ZA", "+30": "GR", "+31": "NL",
	"+32": "BE", "+33": "FR", "+34": "ES", "+36": "HU",
	"+39": "IT", "+40": "RO", "+41": "CH", "+43": "AT",
	"+44": "GB", "+45": "DK", "+46": "SE", "+47": "NO",
	"+48": "PL", "+49": "DE",
	"+51": "PE", "+52": "MX", "+54": "AR", "+55": "BR",
	"+56": "CL", "+57": "CO", "+58": "VE",
	"+60": "MY", "+61": "AU", "+62": "ID", "+63": "PH",
	"+64": "NZ", "+65": "SG", "+66": "TH",
	"+81": "JP", "+82": "KR", "+84": "VN", "+86": "CN",
	"+90": "TR", "+91": "IN", "+92": "PK", "+93": "AF",
	"+94": "LK", "+95": "MM", "+98": "IR",
	// 1-digit codes
	"+1": "US", "+7": "RU",
}

// countryNameToISO maps lowercase country names to ISO 3166-1 alpha-2 codes.
var countryNameToISO = map[string]string{
	"israel": "IL", "afghanistan": "AF", "albania": "AL", "algeria": "DZ",
	"argentina": "AR", "armenia": "AM", "australia": "AU", "austria": "AT",
	"azerbaijan": "AZ", "bahrain": "BH", "bangladesh": "BD", "belarus": "BY",
	"belgium": "BE", "bolivia": "BO", "bosnia": "BA", "brazil": "BR",
	"bulgaria": "BG", "cambodia": "KH", "canada": "CA", "chile": "CL",
	"china": "CN", "colombia": "CO", "croatia": "HR", "czech republic": "CZ",
	"czechia": "CZ", "denmark": "DK", "ecuador": "EC", "egypt": "EG",
	"estonia": "EE", "ethiopia": "ET", "finland": "FI", "france": "FR",
	"georgia": "GE", "germany": "DE", "ghana": "GH", "greece": "GR",
	"hong kong": "HK", "hungary": "HU", "iceland": "IS", "india": "IN",
	"indonesia": "ID", "iran": "IR", "iraq": "IQ", "ireland": "IE",
	"italy": "IT", "japan": "JP", "jordan": "JO", "kazakhstan": "KZ",
	"kenya": "KE", "south korea": "KR", "korea": "KR", "kuwait": "KW",
	"latvia": "LV", "lebanon": "LB", "libya": "LY", "lithuania": "LT",
	"luxembourg": "LU", "malaysia": "MY", "malta": "MT", "mexico": "MX",
	"moldova": "MD", "montenegro": "ME", "morocco": "MA", "myanmar": "MM",
	"nepal": "NP", "netherlands": "NL", "new zealand": "NZ", "nigeria": "NG",
	"north macedonia": "MK", "norway": "NO", "oman": "OM", "pakistan": "PK",
	"peru": "PE", "philippines": "PH", "poland": "PL", "portugal": "PT",
	"qatar": "QA", "romania": "RO", "russia": "RU", "saudi arabia": "SA",
	"senegal": "SN", "serbia": "RS", "singapore": "SG", "slovakia": "SK",
	"slovenia": "SI", "south africa": "ZA", "spain": "ES", "sri lanka": "LK",
	"sweden": "SE", "switzerland": "CH", "syria": "SY", "taiwan": "TW",
	"tanzania": "TZ", "thailand": "TH", "tunisia": "TN", "turkey": "TR",
	"türkiye": "TR", "ukraine": "UA", "united arab emirates": "AE", "uae": "AE",
	"united kingdom": "GB", "uk": "GB", "england": "GB",
	"united states": "US", "usa": "US", "u.s.a.": "US", "u.s.": "US",
	"uruguay": "UY", "uzbekistan": "UZ", "venezuela": "VE", "vietnam": "VN",
	"yemen": "YE", "zambia": "ZM", "zimbabwe": "ZW",
}

// phoneCountryISO returns the ISO 3166-1 alpha-2 code for the applicant's
// phone country.  It first tries to infer from the E.164 dial prefix of
// info.Phone ("+972…" → "IL"), then falls back to info.Country.
func phoneCountryISO(info ApplicantInfo) string {
	if strings.HasPrefix(info.Phone, "+") {
		// Try longest dial code first (4 chars including "+", then 3, then 2).
		for _, n := range []int{4, 3, 2} {
			if len(info.Phone) >= n {
				if iso, ok := dialCodeToISO[info.Phone[:n]]; ok {
					return iso
				}
			}
		}
	}
	if info.Country != "" {
		cl := strings.ToLower(strings.TrimSpace(info.Country))
		if iso, ok := countryNameToISO[cl]; ok {
			return iso
		}
		// Caller may have passed a 2-letter ISO code directly.
		if len(info.Country) == 2 {
			return strings.ToUpper(info.Country)
		}
	}
	return ""
}

// countryDisplayName returns the human-readable country name for a React Select
// combobox (e.g. "Israel", "United States").  If info.Country is a 2-letter ISO
// code it is reverse-looked-up in countryNameToISO; otherwise it is returned
// as-is so user-supplied full names pass through unchanged.
func countryDisplayName(info ApplicantInfo) string {
	c := info.Country
	if c == "" {
		// Infer from phone prefix.
		iso := phoneCountryISO(info)
		if iso == "" {
			return ""
		}
		for name, code := range countryNameToISO {
			if code == iso {
				return titleWords(name)
			}
		}
		return iso
	}
	if len(c) == 2 {
		cu := strings.ToUpper(c)
		for name, code := range countryNameToISO {
			if code == cu {
				return titleWords(name)
			}
		}
	}
	return c
}

// titleWords upper-cases the first letter of each word (ASCII-safe replacement
// for the deprecated strings.Title).
func titleWords(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// fillGreenhouseComboboxByID interacts with a Greenhouse React Select combobox
// by id: clicks it to open the menu, optionally types value to filter, then
// clicks the best-matching option.  When value is "" the dropdown is opened
// without typing and the first option is selected (mop-up mode for unknown
// dropdowns).  waitMs controls how long to wait for options to appear after
// the click/type — use a larger value for async-fetched lists such as the
// Google Places location field.
func fillGreenhouseComboboxByID(wd selenium.WebDriver, id, value string, waitMs int) bool {
	inp, err := wd.FindElement(selenium.ByCSSSelector, "#"+id)
	if err != nil {
		log.Printf("[greenhouse] combobox #%s not found", id)
		return false
	}
	if err := inp.Click(); err != nil {
		return false
	}
	time.Sleep(150 * time.Millisecond)
	// Clear any previously typed text (e.g. from a prior failed fill attempt).
	// inp.Clear() is safer than Ctrl+A+Delete, which React Select can intercept
	// to close the dropdown — causing the first typed character to be swallowed
	// by the re-open event instead of landing in the filter input.
	_ = inp.Clear()
	if value != "" {
		if err := inp.SendKeys(value); err != nil {
			return false
		}
	}
	time.Sleep(time.Duration(waitMs) * time.Millisecond)

	for _, sel := range []string{
		`[role="listbox"] [role="option"]`,
		`[role="option"]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		valLow := strings.ToLower(value)
		for _, el := range els {
			text, _ := el.Text()
			if strings.Contains(strings.ToLower(text), valLow) {
				if err := el.Click(); err == nil {
					log.Printf("[greenhouse] combobox #%s: selected %q", id, strings.TrimSpace(text))
					time.Sleep(200 * time.Millisecond)
					return true
				}
			}
		}
		// No text match — click first option as fallback.
		if err := els[0].Click(); err == nil {
			text, _ := els[0].Text()
			log.Printf("[greenhouse] combobox #%s: first-option fallback %q", id, strings.TrimSpace(text))
			time.Sleep(200 * time.Millisecond)
			return true
		}
	}
	inp.SendKeys(selenium.EscapeKey) //nolint:errcheck
	log.Printf("[greenhouse] combobox #%s: no options appeared for %q", id, value)
	return false
}

// splitDualMajor splits "Computer Science and Mathematics" into
// ["Computer Science", "Mathematics"].  Returns nil for a single major.
// Recognises the separators "and", "&", "/", and "," (in that priority order).
func splitDualMajor(field string) []string {
	low := strings.ToLower(field)
	for _, sep := range []string{" and ", " & ", " / ", "/", ", "} {
		idx := strings.Index(low, sep)
		if idx < 0 {
			continue
		}
		a := strings.TrimSpace(field[:idx])
		b := strings.TrimSpace(field[idx+len(sep):])
		if a != "" && b != "" {
			return []string{a, b}
		}
	}
	return nil
}

// fillGreenhouseComboboxOrOther is like fillGreenhouseComboboxByID but inserts
// an "Other" selection attempt between the value-match pass and the first-option
// fallback.  Use this for school, degree, and field-of-study comboboxes: the
// user's value may not appear in the ATS's curated database list, and blindly
// picking the first option would silently fill the field with wrong data.
//
// Priority order:
//  1. Option whose text contains the typed value (exact data match).
//  2. Option whose text is exactly "Other" / "Other…" (safe unknown catch-all).
//  3. First available option (last resort, same as fillGreenhouseComboboxByID).
func fillGreenhouseComboboxOrOther(wd selenium.WebDriver, id, value string, waitMs int) bool {
	// openAndType clicks the input, clears it, types needle, waits, and returns
	// all visible option elements — or nil when none are found.
	openAndType := func(needle string, wait int) []selenium.WebElement {
		inp, err := wd.FindElement(selenium.ByCSSSelector, "#"+id)
		if err != nil {
			return nil
		}
		if err := inp.Click(); err != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
		_ = inp.Clear()
		if needle != "" {
			_ = inp.SendKeys(needle)
		}
		time.Sleep(time.Duration(wait) * time.Millisecond)
		for _, sel := range []string{`[role="listbox"] [role="option"]`, `[role="option"]`} {
			if els, err := wd.FindElements(selenium.ByCSSSelector, sel); err == nil && len(els) > 0 {
				return els
			}
		}
		return nil
	}

	// Phase 1: search for the actual value.
	if els := openAndType(value, waitMs); els != nil {
		valLow := strings.ToLower(value)
		for _, el := range els {
			text, _ := el.Text()
			if strings.Contains(strings.ToLower(text), valLow) {
				if err := el.Click(); err == nil {
					log.Printf("[greenhouse] combobox #%s: selected %q", id, strings.TrimSpace(text))
					time.Sleep(200 * time.Millisecond)
					return true
				}
			}
		}
	}

	// Phase 1b: dual major — try each component individually so "Computer Science
	// and Mathematics" can still match "Computer Science" when the ATS only lists
	// individual disciplines.
	if parts := splitDualMajor(value); len(parts) > 0 {
		for _, part := range parts {
			if els := openAndType(part, waitMs); els != nil {
				partLow := strings.ToLower(part)
				for _, el := range els {
					text, _ := el.Text()
					if strings.Contains(strings.ToLower(text), partLow) {
						if err := el.Click(); err == nil {
							log.Printf("[greenhouse] combobox #%s: dual major — matched component %q (full: %q)", id, strings.TrimSpace(text), value)
							time.Sleep(200 * time.Millisecond)
							return true
						}
					}
				}
			}
		}
	}

	// Phase 2: value not found — search specifically for "Other".
	if els := openAndType("other", 500); els != nil {
		for _, el := range els {
			text, _ := el.Text()
			low := strings.ToLower(strings.TrimSpace(text))
			if low == "other" || strings.HasPrefix(low, "other ") || strings.HasSuffix(low, " other") {
				if err := el.Click(); err == nil {
					log.Printf("[greenhouse] combobox #%s: %q not in list — fell back to \"Other\"", id, value)
					time.Sleep(200 * time.Millisecond)
					return true
				}
			}
		}
	}

	// Phase 3: no "Other" option either — first-option fallback.
	if els := openAndType(value, waitMs/2); els != nil {
		if err := els[0].Click(); err == nil {
			text, _ := els[0].Text()
			log.Printf("[greenhouse] combobox #%s: first-option fallback %q", id, strings.TrimSpace(text))
			time.Sleep(200 * time.Millisecond)
			return true
		}
	}

	if inp, err := wd.FindElement(selenium.ByCSSSelector, "#"+id); err == nil {
		inp.SendKeys(selenium.EscapeKey) //nolint:errcheck
	}
	log.Printf("[greenhouse] combobox #%s: no options appeared for %q", id, value)
	return false
}

// clickGreenhouseCoverLetterManual looks for an "Enter manually" button inside
// a cover-letter upload widget and clicks it so that a plain-text textarea is
// revealed.  This allows fillGreenhouse to inject cover-letter text into forms
// (e.g. Tailscale on job-boards.greenhouse.io) that require a cover letter but
// offer a text-entry alternative to a file upload.
func clickGreenhouseCoverLetterManual(wd selenium.WebDriver) {
	_, _ = wd.ExecuteScript(`
(function(){
    var btns = document.querySelectorAll('button,a,[role="button"]');
    for (var i = 0; i < btns.length; i++) {
        var t = (btns[i].innerText || btns[i].textContent || '').trim().toLowerCase();
        if (t === 'enter manually' || t === 'enter text manually') {
            // Make sure this button is inside a cover-letter section
            var p = btns[i].parentElement;
            for (var d = 0; d < 8 && p; d++) {
                if ((p.textContent || '').toLowerCase().indexOf('cover') !== -1) {
                    btns[i].click(); return true;
                }
                p = p.parentElement;
            }
        }
    }
    return false;
})()
`, nil)
}

func fillGreenhouse(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd, "#first_name", 8*time.Second)
	first, last := splitName(info.Name)
	if !trySetInput(wd, "#first_name", first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, "#last_name", last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, "#email", info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}

	// Phone country — React Select combobox (id="country"); there is no native
	// <select> in the new Greenhouse form.  We type the country display name
	// (e.g. "Israel") so the autocomplete can filter and we click the match.
	if name := countryDisplayName(info); name != "" {
		fillGreenhouseComboboxByID(wd, "country", name, 800)
	}
	// The country code is handled by the React Select combobox above, not a
	// native <select>, so fillPhone's native-select detection does not fire.
	// Strip the dial code via splitPhone and fill the phone input with the
	// national number directly (e.g. "972556661778" → "0556661778").
	_, ghLocalPhone := splitPhone(info.Phone)
	fillPhoneInput(wd, ghLocalPhone)

	// Location — Google Places-backed async combobox.  The old boards.greenhouse.io
	// format uses id="candidate-location"; the new job-boards.greenhouse.io format
	// may omit this field, use a different id, or embed it as a custom question.
	// Try multiple IDs, then fall back to label-based combobox matching.
	locVal := info.City
	if locVal == "" {
		locVal = countryDisplayName(info)
	}
	if locVal != "" {
		if !fillGreenhouseComboboxByID(wd, "candidate-location", locVal, 1500) {
			if !fillGreenhouseComboboxByID(wd, "location", locVal, 1500) {
				fillComboboxByLabel(wd, "location", locVal)
			}
		}
	}

	// LinkedIn — required on most Greenhouse forms.
	if info.LinkedInURL != "" {
		if !trySetInput(wd, `input[aria-label*="linkedin" i]`, info.LinkedInURL) &&
			!trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
			tryFillByLabel(wd, "linkedin", info.LinkedInURL)
		}
	}

	uploadResume(wd, resumePath)

	// Cover letter — some Greenhouse forms (e.g. Tailscale) require a separate
	// cover letter file upload.  When only text is available, click "Enter
	// manually" to reveal a textarea and fill it with the cover letter text.
	if info.CoverLetter != "" {
		clickGreenhouseCoverLetterManual(wd)
		time.Sleep(500 * time.Millisecond)
		tryFillByLabel(wd, "cover letter", info.CoverLetter)
	}

	fillCommonExtras(ctx, wd, info)

	// ── Greenhouse React Select combobox pass ─────────────────────────────────
	// All Greenhouse custom question dropdowns use React Select comboboxes.
	// fillCommonExtras handles radio buttons and native <select>; these need
	// label-pattern matching.  Needles are purposely specific so they don't
	// accidentally match the phone-country combobox (id="country", label="Country").

	country := countryDisplayName(info)
	authAns := info.WorkAuthorized
	if authAns == "" {
		authAns = "yes"
	}
	sponsorAns := info.RequireSponsorship
	if sponsorAns == "" {
		sponsorAns = "no"
	}

	// Country / residence selection questions (phrasing varies per employer)
	fillComboboxByLabel(wd, "country in which you are located", country)
	fillComboboxByLabel(wd, "country of residence", country)
	fillComboboxByLabel(wd, "current country", country)
	fillComboboxByLabel(wd, "please choose the country", country)

	// Work eligibility
	fillComboboxByLabel(wd, "eligible to work", authAns)
	fillComboboxByLabel(wd, "authorized to work", authAns)
	fillComboboxByLabel(wd, "right to work", authAns)

	// Visa sponsorship (multiple phrasings used by different companies).
	// fillComboboxByLabel handles React Select comboboxes; trySetSelectByLabel
	// covers forms that embed a native <select> (some older Greenhouse variants).
	// Any remaining unfilled dropdowns are caught by the mop-up at the end.
	fillComboboxByLabel(wd, "require sponsorship", sponsorAns)
	fillComboboxByLabel(wd, "sponsorship for a visa", sponsorAns)
	fillComboboxByLabel(wd, "visa to remain", sponsorAns)
	trySetSelectByLabel(wd, "require sponsorship", sponsorAns)
	trySetSelectByLabel(wd, "visa sponsorship", sponsorAns)
	trySetSelectByLabel(wd, "sponsorship to work", sponsorAns)

	// Prior employment and legal restrictions
	fillComboboxByLabel(wd, "previously worked at", "no")
	fillComboboxByLabel(wd, "previously worked for", "no")
	fillComboboxByLabel(wd, "employment agreement", "no")
	fillComboboxByLabel(wd, "post-employment restriction", "no")
	fillComboboxByLabel(wd, "non-disclosure", "no")
	fillComboboxByLabel(wd, "non-compete", "no")
	fillComboboxByLabel(wd, "non-solicitation", "no")
	fillComboboxByLabel(wd, "cooling-off period", "no")
	fillComboboxByLabel(wd, "cool-off period", "no")
	fillComboboxByLabel(wd, "restrictive covenant", "no")
	fillComboboxByLabel(wd, "work restriction", "no")
	fillComboboxByLabel(wd, "confidentiality agreement", "no")
	fillComboboxByLabel(wd, "garden leave", "no")

	// Location eligibility — two common phrasings with different expected answers.
	// "based in the following [locations/countries]" → yes (user qualifies)
	// "based in the United States or Canada" → no (user is outside NA)
	// "located in or willing to relocate to the United States" → no for non-US applicants.
	// Both React combobox and native <select> variants are covered.
	fillComboboxByLabel(wd, "based in the following", "yes")
	fillComboboxByLabel(wd, "united states or canada", "no")
	fillComboboxByLabel(wd, "willing to relocate to the united states", "no")
	fillComboboxByLabel(wd, "located in or willing to relocate", "no")
	trySetSelectByLabel(wd, "willing to relocate to the united states", "no")
	trySetSelectByLabel(wd, "located in or willing to relocate", "no")
	trySetSelectByLabel(wd, "reside in the united states", "no")

	// EEO/demographics — "wish to answer" matches the standard Greenhouse decline
	// phrase "I don't wish to answer" as well as "I prefer not to answer" variants.
	// Covers both numeric IDs (Grafana: 4000681004) and named IDs (GitLab: gender).
	const eeoDecline = "wish to answer"
	fillComboboxByLabel(wd, "gender", eeoDecline)
	fillComboboxByLabel(wd, "hispanic", eeoDecline)
	fillComboboxByLabel(wd, "race", eeoDecline)
	fillComboboxByLabel(wd, "ethnicity", eeoDecline)
	fillComboboxByLabel(wd, "veteran", eeoDecline)
	fillComboboxByLabel(wd, "disability", eeoDecline)
	fillComboboxByLabel(wd, "transgender", eeoDecline)

	// Free-text custom questions common on Greenhouse forms.
	tryFillByLabel(wd, "country and time zone", info.Country)
	// "How did you hear" may be a React Select combobox (new Greenhouse) or a
	// native text input.  Try combobox first; fall back to plain text injection.
	if !fillComboboxByLabel(wd, "how did you hear", "job board") {
		tryFillByLabel(wd, "how did you hear", "Online job board")
	}

	// ── Education ─────────────────────────────────────────────────────────────
	// Greenhouse education fields come in two forms:
	//   • React Select comboboxes (new job-boards.greenhouse.io format)
	//   • Native <select> elements (older boards.greenhouse.io format)
	// The mop-up below catches the combobox variant; handle native <select>
	// and text inputs here so both formats are covered.
	if info.School != "" {
		// Combobox: try exact school name; fall back to "Other" if not in list.
		fillGreenhouseComboboxOrOther(wd, "school_name_0", info.School, 900)
		// Native <select> and text-input variants (older Greenhouse format).
		if !trySetSelect(wd, `select[id*="school" i], select[name*="school" i]`, info.School) {
			trySetSelectByLabel(wd, "school", info.School)
		}
		tryFillByLabel(wd, "school", info.School)
		tryFillByLabel(wd, "institution", info.School)
	}
	if info.Degree != "" {
		degreeTerms := map[string][]string{
			"bachelor":  {"bachelor"},
			"master":    {"master"},
			"phd":       {"doctoral", "ph.d", "doctor"},
			"associate": {"associate"},
		}
		for _, term := range degreeTerms[info.Degree] {
			if fillGreenhouseComboboxOrOther(wd, "degree_0", term, 800) {
				break
			}
		}
		for _, term := range degreeTerms[info.Degree] {
			if trySetSelect(wd, `select[id*="degree" i], select[name*="degree" i]`, term) {
				break
			}
			if trySetSelectByLabel(wd, "degree", term) {
				break
			}
		}
	}
	if info.FieldOfStudy != "" {
		// Combobox: try field name; fall back to "Other" if not in list.
		fillGreenhouseComboboxOrOther(wd, "discipline_0", info.FieldOfStudy, 800)
		tryFillByLabel(wd, "field of study", info.FieldOfStudy)
		tryFillByLabel(wd, "major", info.FieldOfStudy)
		tryFillByLabel(wd, "discipline", info.FieldOfStudy)
		trySetSelectByLabel(wd, "field of study", info.FieldOfStudy)
		trySetSelectByLabel(wd, "major", info.FieldOfStudy)
	}

	// Mop-up pass: find every remaining unfilled React Select combobox using
	// the visible-placeholder heuristic (works with both the old .select-shell
	// pattern and the new job-boards.greenhouse.io format that uses different
	// container classes).  For each, choose a value based on its label text.
	fillGreenhouseAllUnfilledDropdowns(wd, sponsorAns, authAns, info)
}

// fillGreenhouseEEODecline selects the "decline to answer" option from an EEO
// React Select combobox.  Different ATS instances phrase the decline option
// differently ("I don't wish to answer", "Prefer not to say", "Decline to
// self-identify", etc.), so we try several needles.  If none match, we open
// the dropdown with all options visible and click the last one — Greenhouse EEO
// forms always place the decline option last.
func fillGreenhouseEEODecline(wd selenium.WebDriver, id string) {
	for _, needle := range []string{
		"decline",       // "Decline To Self Identify" (most common Greenhouse phrasing)
		"wish to answer", // "I don't wish to answer"
		"prefer not",   // "I prefer not to answer"
		"no response",
		"not to disclose",
		"choose not",
	} {
		if fillGreenhouseComboboxByID(wd, id, needle, 800) {
			return
		}
	}
	// All typed needles failed — open the dropdown with no filter text and
	// click the last visible option (always the decline entry in GH EEO forms).
	inp, err := wd.FindElement(selenium.ByCSSSelector, "#"+id)
	if err != nil {
		return
	}
	if err := inp.Click(); err != nil {
		return
	}
	// Clear + no typing = show all options
	_ = inp.Clear()
	time.Sleep(600 * time.Millisecond)
	for _, sel := range []string{`[role="listbox"] [role="option"]`, `[role="option"]`} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		last := els[len(els)-1]
		if err := last.Click(); err == nil {
			text, _ := last.Text()
			log.Printf("[greenhouse] EEO combobox #%s: last-option decline %q", id, strings.TrimSpace(text))
			time.Sleep(200 * time.Millisecond)
			return
		}
	}
	inp.SendKeys(selenium.EscapeKey) //nolint:errcheck
}

// fillGreenhouseAllUnfilledDropdowns finds every React Select combobox on the
// page that still shows a placeholder (= nothing selected) and fills it with
// an answer chosen by label-text matching.  This covers both the old
// boards.greenhouse.io format (.select-shell wrappers) and the new
// job-boards.greenhouse.io format (no .select-shell, different container class)
// which was previously missed by jsGetUnfilledComboboxIDs.
func fillGreenhouseAllUnfilledDropdowns(wd selenium.WebDriver, sponsorAns, authAns string, info ApplicantInfo) {
	res, err := wd.ExecuteScript(jsGetUnfilledComboboxDetails, nil)
	if err != nil || res == nil {
		return
	}
	pairs, ok := res.([]interface{})
	if !ok || len(pairs) == 0 {
		return
	}

	const eeoDecline = "wish to answer"

	// Map normalised degree keyword → search term that matches Greenhouse's
	// degree dropdown options (e.g. "bachelor" matches "Bachelor's Degree").
	degreeSearch := map[string]string{
		"bachelor":  "bachelor",
		"master":    "master",
		"phd":       "doctoral",
		"associate": "associate",
	}

	for _, item := range pairs {
		pair, ok := item.([]interface{})
		if !ok || len(pair) < 2 {
			continue
		}
		id, _ := pair[0].(string)
		lbl, _ := pair[1].(string)
		if id == "" {
			continue
		}

		var value string
		switch {
		case strings.Contains(lbl, "non-disclosure") ||
			strings.Contains(lbl, "non-compete") ||
			strings.Contains(lbl, "non-solicitation") ||
			strings.Contains(lbl, "cooling-off") ||
			strings.Contains(lbl, "cool-off") ||
			strings.Contains(lbl, "restrictive covenant") ||
			strings.Contains(lbl, "work restriction") ||
			strings.Contains(lbl, "confidentiality agreement") ||
			strings.Contains(lbl, "garden leave") ||
			strings.Contains(lbl, "post-employment"):
			value = "no"
		case strings.Contains(lbl, "relocat") ||
			strings.Contains(lbl, "reside") ||
			strings.Contains(lbl, "located in") ||
			strings.Contains(lbl, "united states or canada") ||
			strings.Contains(lbl, "north america"):
			value = "no"
		case strings.Contains(lbl, "sponsor") ||
			strings.Contains(lbl, "visa") ||
			strings.Contains(lbl, "immigration"):
			value = sponsorAns
		case strings.Contains(lbl, "authorized") ||
			strings.Contains(lbl, "authorised") ||
			strings.Contains(lbl, "right to work") ||
			strings.Contains(lbl, "eligible to work"):
			value = authAns
		case strings.Contains(lbl, "gender") ||
			strings.Contains(lbl, "sex ") ||
			strings.Contains(lbl, "hispanic") ||
			strings.Contains(lbl, "ethnicity") ||
			strings.Contains(lbl, "race ") ||
			strings.Contains(lbl, "racial") ||
			strings.Contains(lbl, "veteran") ||
			strings.Contains(lbl, "disability"):
			log.Printf("[greenhouse] mop-up: EEO combobox #%s label=%q → decline", id, lbl)
			fillGreenhouseEEODecline(wd, id)
			continue

		case strings.Contains(lbl, "school") ||
			strings.Contains(lbl, "institution") ||
			strings.Contains(lbl, "university") ||
			strings.Contains(lbl, "college"):
			v := info.School
			log.Printf("[greenhouse] mop-up: school combobox #%s label=%q → %q (other fallback enabled)", id, lbl, v)
			fillGreenhouseComboboxOrOther(wd, id, v, 1200)
			continue

		case strings.Contains(lbl, "degree") ||
			strings.Contains(lbl, "qualification") ||
			strings.Contains(lbl, "education level"):
			term := degreeSearch[info.Degree] // "" when degree unknown → other fallback
			log.Printf("[greenhouse] mop-up: degree combobox #%s label=%q → %q (other fallback enabled)", id, lbl, term)
			fillGreenhouseComboboxOrOther(wd, id, term, 1000)
			continue

		case strings.Contains(lbl, "field of study") ||
			strings.Contains(lbl, "major") ||
			strings.Contains(lbl, "discipline") ||
			strings.Contains(lbl, "area of study"):
			v := info.FieldOfStudy
			log.Printf("[greenhouse] mop-up: field combobox #%s label=%q → %q (other fallback enabled)", id, lbl, v)
			fillGreenhouseComboboxOrOther(wd, id, v, 1000)
			continue

		default:
			value = "" // first-option fallback
		}

		log.Printf("[greenhouse] mop-up: combobox #%s label=%q → %q", id, lbl, value)
		fillGreenhouseComboboxByID(wd, id, value, 800)
	}
}

func fillLever(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd, `input[name="name"]`, 8*time.Second)
	first, last := splitName(info.Name)
	if !trySetInput(wd, `input[name="name"]`, info.Name) {
		tryFillByLabel(wd, "full name", info.Name)
	}
	if !trySetInput(wd, `input[placeholder*="First" i]`, first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, `input[placeholder*="Last" i]`, last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, `input[name="email"]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	fillPhone(wd, info.Phone)
	if !trySetInput(wd, `input[name="urls[LinkedIn]"]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

// jsGetAshbyUnfilledComboboxes returns [[inputId, labelText], ...] for every
// visible Ashby combobox that currently has no selected value.  It detects the
// empty state by checking whether the placeholder element is still visible
// inside the combobox container (Ashby hides the placeholder once a value is
// chosen) or whether the input value is empty.
const jsGetAshbyUnfilledComboboxes = `
try {
    var res = [];
    var seen = {};
    var inputs = document.querySelectorAll('input[role="combobox"]');
    for (var i = 0; i < inputs.length; i++) {
        var inp = inputs[i];
        if (!inp.id || seen[inp.id] || inp.disabled) continue;
        // Walk up to find the combobox container (usually a div with a class
        // that contains "select", "combobox", or "dropdown").
        var unfilled = false;
        var p = inp.parentElement;
        for (var d = 0; d < 8 && p; d++) {
            // Visible placeholder means no value selected.
            var ph = p.querySelector('[class*="placeholder" i]');
            if (ph) {
                var st = window.getComputedStyle(ph);
                if (st.display !== 'none' && parseFloat(st.opacity || '1') > 0.1) {
                    unfilled = true;
                    break;
                }
            }
            // Also check: singleValue element absent means no selection.
            var sv = p.querySelector('[class*="single-value" i], [class*="singleValue" i]');
            if (sv && window.getComputedStyle(sv).display !== 'none') {
                unfilled = false; // has a selected value
                break;
            }
            p = p.parentElement;
        }
        if (!unfilled) continue;
        seen[inp.id] = true;
        // Climb from the input to find its label.
        var lbl = '';
        var c = inp.parentElement;
        for (var j = 0; j < 12 && c; j++) {
            var lel = c.querySelector('label');
            if (lel) { lbl = lel.textContent.trim().toLowerCase(); break; }
            c = c.parentElement;
        }
        res.push([inp.id, lbl]);
    }
    return res;
} catch(e) { return []; }
`

// fillAshbyUnfilledComboboxes finds every Ashby combobox that still has no
// selected value after the main fill pass and selects its first available
// option.  This catches employer-specific custom questions whose label text
// doesn't match any of the targeted patterns above.
func fillAshbyUnfilledComboboxes(wd selenium.WebDriver) {
	raw, err := wd.ExecuteScript(jsGetAshbyUnfilledComboboxes, nil)
	if err != nil || raw == nil {
		return
	}
	pairs, ok := raw.([]interface{})
	if !ok {
		return
	}
	for _, item := range pairs {
		pair, ok := item.([]interface{})
		if !ok || len(pair) < 2 {
			continue
		}
		id, _ := pair[0].(string)
		lbl, _ := pair[1].(string)
		if id == "" {
			continue
		}
		log.Printf("[ashby] mop-up: unfilled combobox #%s label=%q → first option", id, lbl)
		inp, err := wd.FindElement(selenium.ByCSSSelector, "#"+id)
		if err != nil {
			continue
		}
		if err := inp.Click(); err != nil {
			continue
		}
		time.Sleep(500 * time.Millisecond)
		for _, sel := range []string{`[role="listbox"] [role="option"]`, `[role="option"]`} {
			opts, err := wd.FindElements(selenium.ByCSSSelector, sel)
			if err != nil || len(opts) == 0 {
				continue
			}
			if err := opts[0].Click(); err == nil {
				text, _ := opts[0].Text()
				log.Printf("[ashby] mop-up: combobox #%s selected first option %q", id, strings.TrimSpace(text))
				time.Sleep(200 * time.Millisecond)
			}
			break
		}
	}
}

func fillAshby(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string, job *JobInfo) {
	// Wait for the name or email field — whichever appears first.
	// Using a compound selector so we're not blocked on a single ID/name variant.
	waitForElement(ctx, wd,
		`input[name="name"], input[autocomplete="name"], input[type="email"], input[autocomplete="email"]`,
		12*time.Second)

	// Name — try every known Ashby variant before falling back to label text.
	if !trySetInput(wd, `input[name="name"]`, info.Name) &&
		!trySetInput(wd, `input[autocomplete="name"]`, info.Name) &&
		!trySetInput(wd, `input[placeholder*="name" i]`, info.Name) {
		if !tryFillByLabel(wd, "full name", info.Name) {
			tryFillByLabel(wd, "name", info.Name)
		}
	}
	// Email
	if !trySetInput(wd, `input[name="email"]`, info.Email) &&
		!trySetInput(wd, `input[type="email"]`, info.Email) &&
		!trySetInput(wd, `input[autocomplete="email"]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	// Phone — Ashby uses UUID-named tel inputs that may ignore the native JS
	// value setter (custom phone component).  Use WebDriver SendKeys first so
	// real key events are dispatched; fall back to JS-setter approaches.
	fillPhoneSendKeys(wd, info.Phone)
	// LinkedIn
	if !trySetInput(wd, `input[placeholder*="LinkedIn" i]`, info.LinkedInURL) &&
		!trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	// Location — Ashby uses a React-Select combobox.  Several label phrasings
	// are tried in order; Pleo uses "Where are you currently located?" (needle
	// "currently located") while others use plain "Location" or "Country".
	locVal := info.City
	if locVal == "" {
		locVal = info.Country
	}
	if locVal != "" {
		if !fillComboboxByLabel(wd, "currently located", locVal) &&
			!fillComboboxByLabel(wd, "where are you currently", locVal) &&
			!fillComboboxByLabel(wd, "location", locVal) {
			fillComboboxByLabel(wd, "country", locVal)
		}
	}
	fillCommonExtras(ctx, wd, info)

	// ── Ashby React Select comboboxes ─────────────────────────────────────────
	// jsHandleWorkEligibility (called via fillCommonExtras) handles native
	// <select> and radio buttons.  Ashby also renders work-auth and sponsorship
	// as React Select comboboxes, which need the click+type approach.
	authAns := info.WorkAuthorized
	if authAns == "" {
		authAns = "yes"
	}
	sponsorAns := info.RequireSponsorship
	if sponsorAns == "" {
		sponsorAns = "no"
	}
	fillComboboxByLabel(wd, "eligible to work", authAns)
	fillComboboxByLabel(wd, "authorized to work", authAns)
	fillComboboxByLabel(wd, "legally allowed to work", authAns)
	fillComboboxByLabel(wd, "right to work", authAns)
	// EU / country-specific work auth phrasing.
	fillComboboxByLabel(wd, "eligible to work in germany", authAns)
	fillComboboxByLabel(wd, "right to work in germany", authAns)
	fillComboboxByLabel(wd, "eligible to work in the eu", authAns)
	fillComboboxByLabel(wd, "right to work in the eu", authAns)
	fillComboboxByLabel(wd, "eligible to work in the uk", authAns)
	fillComboboxByLabel(wd, "right to work in the uk", authAns)
	fillComboboxByLabel(wd, "require sponsorship", sponsorAns)
	fillComboboxByLabel(wd, "sponsorship for a visa", sponsorAns)
	fillComboboxByLabel(wd, "require a visa", sponsorAns)
	fillComboboxByLabel(wd, "need sponsorship", sponsorAns)
	fillComboboxByLabel(wd, "require work permit", sponsorAns)
	fillComboboxByLabel(wd, "require a work permit", sponsorAns)

	// jobInIsrael is used later for "nature of right to work" radio selection.
	descLow := strings.ToLower(job.Description + " " + job.Title)
	jobInIsrael := strings.Contains(descLow, "israel") ||
		strings.Contains(descLow, "tel aviv") ||
		strings.Contains(descLow, "jerusalem") ||
		strings.Contains(descLow, "haifa") ||
		strings.Contains(descLow, "nazareth")

	// "Which location are you applying for?" — employer-defined list of cities/
	// regions.  The applicant may not be in any listed city, so pick "Other"
	// which is always present as a catch-all and does not require sponsorship.
	fillComboboxByLabel(wd, "location are you applying for", "other")
	// Some Ashby forms reveal a free-text input after "Other" is selected.
	// Give React 800 ms to render the conditional field, then fill it.
	time.Sleep(500 * time.Millisecond)
	fillLoc := info.City
	if fillLoc == "" {
		fillLoc = info.Country
	}
	tryFillByLabel(wd, "enter your location below", fillLoc)
	tryFillByLabel(wd, "please enter your location", fillLoc)
	tryFillByLabel(wd, "selected 'other'", fillLoc)

	// "What state would you like to be based in?" — The Zebra and similar
	// US-centric employers ask for an intra-country location via a city/region
	// combobox.  Try the city first; "remote" and "other" are safe fallbacks.
	if !fillComboboxByLabel(wd, "state would you like to be based", info.City) {
		if !fillComboboxByLabel(wd, "state would you like to be based", "remote") {
			fillComboboxByLabel(wd, "state would you like to be based", "other")
		}
	}

	// "What is the nature of your right to work?" — some employers render this
	// as a multi-option radio group (not a yes/no or combobox).  Pick the option
	// whose text contains the appropriate needle.
	var rtWNeedle string
	switch {
	case jobInIsrael:
		rtWNeedle = "citizen of the country"
	case sponsorAns == "yes":
		rtWNeedle = "sponsorship"
	default:
		rtWNeedle = "unlimited"
	}
	wd.ExecuteScript(jsSelectRadioContaining, []interface{}{"nature of your right to work", rtWNeedle}) //nolint:errcheck

	// Boolean Yes/No button-group fields — Ashby renders these as styled <button>
	// pairs, not radio inputs.  jsHandleWorkEligibility handles native select/radio;
	// jsHandleAshbyYesNo covers the button-group variant.
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"authorized to work", authAns})        //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"legally authorized to work", authAns}) //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"legally allowed to work", authAns})   //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"require sponsorship", sponsorAns})    //nolint:errcheck

	// NDA / non-compete / cooling-off / work restrictions — always "no"
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"non-disclosure", "no"})       //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"non-compete", "no"})          //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"non-solicitation", "no"})     //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"cooling-off", "no"})          //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"cool-off", "no"})             //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"work restriction", "no"})     //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"confidentiality agreement", "no"}) //nolint:errcheck
	wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"restrictive covenant", "no"}) //nolint:errcheck
	fillComboboxByLabel(wd, "non-disclosure", "no")
	fillComboboxByLabel(wd, "non-compete", "no")
	fillComboboxByLabel(wd, "non-solicitation", "no")
	fillComboboxByLabel(wd, "cooling-off", "no")
	fillComboboxByLabel(wd, "work restriction", "no")

	// "How did you hear about us?" — Ashby MultiValueSelect (checkbox list).
	// Click the combobox open and pick the first available option.
	wd.ExecuteScript(jsClickFirstMultiValueOption, []interface{}{"how did you hear"}) //nolint:errcheck

	// Required motivation / cover-letter textareas common on Ashby forms.
	// Covers labels like "Why do you want to join X?", "Why this role?", etc.
	// fillCommonExtras already handles `textarea[name*="cover"]`; these match
	// the free-text motivation questions Ashby employers frequently make required.
	for _, lbl := range []string{
		"why do you want to join",
		"why do you want to work",
		"why are you excited",
		"why are you interested",
		"why this role",
		"why this company",
		"what motivates you",
		"tell us about yourself",
		"what interests you about",
	} {
		tryFillByLabel(wd, lbl, info.CoverLetter)
	}

	// Language proficiency — common on European/DACH-region roles (Munich, Berlin,
	// Vienna, Zurich).  Default answer is "Fluent"; selecting the first available
	// option covers custom scales the employer might use.
	for _, lbl := range []string{
		"german language",
		"level of german",
		"german proficiency",
		"deutsch",
		"sprachkenntnisse",
		"english language",
		"level of english",
		"english proficiency",
	} {
		// Try "Fluent" first; fall through to first option via empty-string trick.
		if !fillComboboxByLabel(wd, lbl, "fluent") {
			fillComboboxByLabel(wd, lbl, "native")
		}
	}

	// "Are you currently based in [country/region]?" — Boolean yes/no combobox
	// or button-group.  Answer yes (user is assumed to be applying from within
	// the region or willing to move; sponsorship=no covers no-visa scenarios).
	for _, lbl := range []string{
		"currently based in germany",
		"currently located in germany",
		"currently based in europe",
		"currently located in europe",
		"currently based in the eu",
		"currently located in the eu",
		"based in the uk",
		"located in the uk",
		"reside in germany",
		"reside in the eu",
		"currently reside in germany",
	} {
		fillComboboxByLabel(wd, lbl, "yes")
		wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{lbl, "yes"}) //nolint:errcheck
	}

	// Mop-up: find any remaining required Ashby comboboxes that none of the
	// targeted fills above matched, and select their first available option.
	// This catches employer-specific custom questions with unusual phrasing.
	fillAshbyUnfilledComboboxes(wd)

	// Upload resume last — Ashby's React re-renders triggered by combobox fills
	// can reset the file input widget if the upload happens earlier in the fill
	// sequence.  Uploading after all other state changes prevents this reset.
	//
	// Ashby renders TWO <input type="file"> elements:
	//   input 0  — visual React component (required=false); hit by uploadResume.
	//   input#_systemfield_resume — form submission field (required=true); must
	//   also be populated or Ashby's server-side validation rejects the submit.
	uploadResume(wd, resumePath)
	if resumePath != "" {
		if absPath, err := filepath.Abs(resumePath); err == nil {
			_, _ = wd.ExecuteScript(jsRevealFileInputs, nil)
			time.Sleep(150 * time.Millisecond)
			if sysEls, _ := wd.FindElements(selenium.ByCSSSelector, `input#_systemfield_resume`); len(sysEls) > 0 {
				if kerr := sysEls[0].SendKeys(absPath); kerr == nil {
					log.Printf("[upload] Ashby _systemfield_resume uploaded: %s", filepath.Base(absPath))
				} else {
					log.Printf("[upload] Ashby _systemfield_resume SendKeys failed: %v", kerr)
				}
			}
		}
	}
}

func fillBambooHR(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd,
		`input[id*="firstName" i], input[name*="first" i], input[type="email"]`,
		8*time.Second)
	first, last := splitName(info.Name)
	if !trySetInput(wd, `input[id*="firstName" i]`, first) &&
		!trySetInput(wd, `input[name*="first" i]`, first) &&
		!trySetInput(wd, `input[placeholder*="first" i]`, first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, `input[id*="lastName" i]`, last) &&
		!trySetInput(wd, `input[name*="last" i]`, last) &&
		!trySetInput(wd, `input[placeholder*="last" i]`, last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, `input[id*="email" i]`, info.Email) &&
		!trySetInput(wd, `input[type="email"]`, info.Email) &&
		!trySetInput(wd, `input[name*="email" i]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	fillPhone(wd, info.Phone)
	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

// fillPersonio fills Personio career-page application forms.
// Personio forms are React SPAs that require a longer wait, have known field
// names (first_name / firstName, salary_expectations, available_from), and
// always include a mandatory GDPR/privacy consent checkbox.
func fillPersonio(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	// React form takes longer to render than static ATS pages.
	waitForElement(ctx, wd,
		`input[name="first_name"], input[name="firstName"], `+
			`input[autocomplete="given-name"], input[placeholder*="First" i]`,
		15*time.Second)

	first, last := splitName(info.Name)

	// First name
	if !trySetInput(wd, `input[name="first_name"]`, first) &&
		!trySetInput(wd, `input[name="firstName"]`, first) &&
		!trySetInput(wd, `input[autocomplete="given-name"]`, first) &&
		!trySetInput(wd, `input[placeholder*="First name" i]`, first) {
		if !tryFillByLabel(wd, "first name", first) {
			tryFillByLabel(wd, "vorname", first)
		}
	}
	// Last name
	if !trySetInput(wd, `input[name="last_name"]`, last) &&
		!trySetInput(wd, `input[name="lastName"]`, last) &&
		!trySetInput(wd, `input[autocomplete="family-name"]`, last) &&
		!trySetInput(wd, `input[placeholder*="Last name" i]`, last) {
		if !tryFillByLabel(wd, "last name", last) {
			tryFillByLabel(wd, "nachname", last)
		}
	}
	// Email
	if !trySetInput(wd, `input[name="email"]`, info.Email) &&
		!trySetInput(wd, `input[type="email"]`, info.Email) &&
		!trySetInput(wd, `input[autocomplete="email"]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	// Phone
	fillPhone(wd, info.Phone)
	// LinkedIn
	if !trySetInput(wd, `input[name="linkedin_profile"]`, info.LinkedInURL) &&
		!trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	// Salary expectations — always set (defaults to "Negotiable").
	if !trySetInput(wd, `input[name="salary_expectations"]`, info.ExpectedSalary) &&
		!trySetInput(wd, `input[name="salaryExpectations"]`, info.ExpectedSalary) &&
		!trySetInput(wd, `input[name*="salary" i]`, info.ExpectedSalary) {
		if !tryFillByLabel(wd, "salary expectations", info.ExpectedSalary) {
			tryFillByLabel(wd, "gehaltsvorstellung", info.ExpectedSalary)
		}
	}
	// Earliest start date — always set (defaults to today + 14 days).
	if !trySetInput(wd, `input[name="available_from"]`, info.EarliestStartDate) &&
		!trySetInput(wd, `input[name="availabilityDate"]`, info.EarliestStartDate) &&
		!trySetInput(wd, `input[name="availability_date"]`, info.EarliestStartDate) &&
		!trySetInput(wd, `input[type="date"][name*="available" i]`, info.EarliestStartDate) {
		if !tryFillByLabel(wd, "earliest start date", info.EarliestStartDate) &&
			!tryFillByLabel(wd, "start date", info.EarliestStartDate) {
			tryFillByLabel(wd, "frühestmöglicher eintrittstermin", info.EarliestStartDate)
		}
	}
	uploadResume(wd, resumePath)
	// fillCommonExtras handles city/country, cover letter, work auth, EEO,
	// privacy consent checkbox, salary (JS fallback), and availability (JS fallback).
	fillCommonExtras(ctx, wd, info)
}

func fillGeneric(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd, `input[type="text"], input[type="email"], input[name]`, 8*time.Second)
	first, last := splitName(info.Name)

	// For each field: try attribute-based selectors first, then label-based
	// fallback for React/Vue forms that expose no standard name/id attributes.
	filled := false
	for _, sel := range []string{
		`input[name*="first" i]`, `input[placeholder*="First name" i]`,
		`input[id*="first" i]`, `input[aria-label*="First" i]`,
	} {
		if trySetInput(wd, sel, first) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "first name", first) &&
			!tryFillByLabel(wd, "given name", first) {
			tryFillByLabel(wd, "vorname", first)
		}
	}

	filled = false
	for _, sel := range []string{
		`input[name*="last" i]`, `input[placeholder*="Last name" i]`,
		`input[id*="last" i]`, `input[aria-label*="Last" i]`,
	} {
		if trySetInput(wd, sel, last) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "last name", last) &&
			!tryFillByLabel(wd, "family name", last) {
			if !tryFillByLabel(wd, "nachname", last) {
				tryFillByLabel(wd, "familienname", last)
			}
		}
	}

	filled = false
	for _, sel := range []string{
		`input[name="name"]`, `input[placeholder*="Full name" i]`,
		`input[placeholder*="Your name" i]`,
	} {
		if trySetInput(wd, sel, info.Name) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "full name", info.Name) &&
			!tryFillByLabel(wd, "your name", info.Name) {
			tryFillByLabel(wd, "vollständiger name", info.Name)
		}
	}

	filled = false
	for _, sel := range []string{
		`input[type="email"]`, `input[name*="email" i]`,
		`input[placeholder*="email" i]`, `input[id*="email" i]`,
	} {
		if trySetInput(wd, sel, info.Email) {
			filled = true
			break
		}
	}
	if !filled {
		tryFillByLabel(wd, "email", info.Email)
	}

	fillPhone(wd, info.Phone)

	filled = false
	for _, sel := range []string{
		`input[placeholder*="LinkedIn" i]`, `input[name*="linkedin" i]`,
		`input[id*="linkedin" i]`, `input[aria-label*="LinkedIn" i]`,
	} {
		if trySetInput(wd, sel, info.LinkedInURL) {
			filled = true
			break
		}
	}
	if !filled {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}

	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

// fillWorkday fills Workday ATS application forms.
// Workday uses data-automation-id attributes exclusively — standard name/id/type
// selectors do not work.  This filler is only reached when the user has saved
// Workday credentials in --profile-dir; otherwise the sign-in wall detection in
// FillApplication returns an error before the switch is reached.
func fillWorkday(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd,
		`[data-automation-id="firstName"], [data-automation-id="lastName"]`,
		12*time.Second)

	first, last := splitName(info.Name)

	trySetInput(wd, `[data-automation-id="firstName"]`, first)
	trySetInput(wd, `[data-automation-id="lastName"]`, last)

	// Email — only fill when there is no password field visible (application
	// form context, not sign-in).  The sign-in page also has data-automation-id="email".
	passEls, _ := wd.FindElements(selenium.ByCSSSelector, `[data-automation-id="password"]`)
	if len(passEls) == 0 {
		trySetInput(wd, `[data-automation-id="email"]`, info.Email)
	}

	fillPhone(wd, info.Phone)
	trySetInput(wd, `[data-automation-id="city"]`, info.City)
	trySetInput(wd, `[data-automation-id="postalCode"]`, info.ZipCode)
	trySetSelectByLabel(wd, "country", info.Country)

	if info.LinkedInURL != "" {
		trySetInput(wd, `[data-automation-id="linkedIn"]`, info.LinkedInURL)
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}

	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

// fillICIMS fills iCIMS ATS application forms.
// iCIMS uses standard HTML inputs for text fields but often hides the resume
// file input inside an iframe.  The ?mode=apply URL navigation in FillApplication
// ensures we land directly on the form rather than the job description page.
func fillICIMS(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd,
		`input[type="email"], input[name*="email" i], input[id*="email" i]`,
		10*time.Second)

	first, last := splitName(info.Name)

	// iCIMS field names vary by employer config; try the most common variants.
	if !trySetInput(wd, `input[name="firstname"]`, first) &&
		!trySetInput(wd, `input[name*="first" i]`, first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, `input[name="lastname"]`, last) &&
		!trySetInput(wd, `input[name*="last" i]`, last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, `input[name="email"]`, info.Email) &&
		!trySetInput(wd, `input[type="email"]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	fillPhone(wd, info.Phone)
	trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL)

	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

// fillWorkable fills Workable ATS application forms.
// Workable uses standard HTML inputs; the resume is uploaded via a styled
// drag-drop zone that hides the real <input type="file">.  jsRevealFileInputs
// in uploadResume handles the reveal.  Some Workable pages require clicking a
// visible "Upload Resume" zone first to bring the hidden file input into the DOM.
func fillWorkable(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd,
		`input[type="email"], input[name*="email" i], input[id*="email" i]`,
		10*time.Second)

	first, last := splitName(info.Name)

	if !trySetInput(wd, `input[name*="first" i]`, first) &&
		!trySetInput(wd, `input[placeholder*="First" i]`, first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, `input[name*="last" i]`, last) &&
		!trySetInput(wd, `input[placeholder*="Last" i]`, last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, `input[type="email"]`, info.Email) &&
		!trySetInput(wd, `input[name*="email" i]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	fillPhone(wd, info.Phone)
	if info.LinkedInURL != "" {
		if !trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
			tryFillByLabel(wd, "linkedin", info.LinkedInURL)
		}
	}

	// Workable sometimes renders a drag-drop upload zone that lazy-creates the
	// real <input type="file"> only after the zone is clicked.  Attempt a click
	// before the upload so the input is present when jsRevealFileInputs runs.
	for _, sel := range []string{
		`[class*="upload" i][class*="resume" i]`,
		`[class*="resume" i] [class*="upload" i]`,
		`[data-ui*="upload" i]`,
		`button[class*="upload" i]`,
		`.drop-zone`, `[class*="dropzone" i]`,
		`[class*="file-upload" i]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			_ = els[0].Click()
			time.Sleep(400 * time.Millisecond)
			break
		}
	}

	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// jsAcceptCookies dismisses cookie consent banners in two passes:
//  1. Exact CSS selectors for well-known consent libraries (Cookiebot, OneTrust,
//     Osano, Didomi, TrustArc, Cookie Script, Usercentrics, Funding Choices, …).
//  2. Text-based matching inside any element whose class/id contains "cookie",
//     "consent", "gdpr", "banner", "notice", or "cc-".  Accept-words cover
//     English, German, Dutch, Finnish, Norwegian, Swedish, French, and Danish.
//     Buttons whose text also contains a reject-word are skipped.
//
// Returns true when a button was clicked so the Go side can wait briefly for
// the overlay animation to finish before continuing.
const jsAcceptCookies = `
(function() {
    // Pass 1: library-specific selectors.
    var exact = [
        '#CybotCookiebotDialogBodyLevelButtonLevelOptinAllowAll',
        '#CybotCookiebotDialogBodyButtonAccept',
        '#onetrust-accept-btn-handler',
        '.onetrust-accept-btn-handler',
        '.osano-cm-accept-all', '.osano-cm-accept',
        '#didomi-notice-agree-button',
        '#truste-consent-button',
        '#cookiescript_accept_all', '#cookiescript_accept',
        'button[data-testid="uc-accept-all-button"]',
        '.fc-button.fc-cta-consent',
        '#coiConsentBannerFooterButton',
        '[data-cookiebanner="accept_button"]',
        '#accept-cookies', '#cookie-accept', '#acceptCookies',
        '#accept-all-cookies', '#acceptAllCookies',
        '.js-cookie-accept', '.js-accept-cookies',
        'button[class*="cookie-consent-accept" i]',
        'button[class*="accept-cookie" i]',
        'button[class*="accept-all-cookie" i]',
        'button[aria-label*="accept all" i]',
        'button[aria-label*="alle akzeptieren" i]',
        'button[aria-label*="acceptera alla" i]',
    ];
    for (var s = 0; s < exact.length; s++) {
        try {
            var el = document.querySelector(exact[s]);
            if (el && !el.disabled) { el.click(); return true; }
        } catch(e) {}
    }

    // Pass 2: multilingual text matching inside consent containers.
    var ACCEPT = [
        // English
        'accept', 'allow all', 'allow cookies', 'agree', 'i agree',
        'i accept', 'ok', 'got it',
        // German
        'akzeptieren', 'zustimmen', 'einverstanden', 'annehmen', 'ich stimme',
        // Dutch
        'accepteren', 'akkoord', 'toestaan', 'instemmen',
        // Finnish
        'hyväksy',
        // Norwegian
        'godta', 'aksepter',
        // Swedish
        'acceptera', 'godkänn',
        // French
        "j'accepte", 'accepter',
        // Danish
        'godkend',
    ];
    var REJECT = [
        'decline', 'reject', 'refuse', 'deny',
        'necessary only', 'only necessary', 'essential only',
        'ablehnen', 'nein', 'nur notwendige', 'nur erforderliche',
        'weigeren', 'alleen noodzakelijke',
        'avvisa', 'avvis', 'neka',
        'hylkää',
    ];
    function textOf(el) {
        return (el.innerText || el.value || el.getAttribute('aria-label') || el.title || '').trim().toLowerCase();
    }
    function hasWord(t, list) {
        for (var i = 0; i < list.length; i++) if (t.indexOf(list[i]) !== -1) return true;
        return false;
    }
    var containerSel = [
        '[class*="cookie" i]', '[id*="cookie" i]',
        '[class*="consent" i]', '[id*="consent" i]',
        '[class*="gdpr" i]', '[id*="gdpr" i]',
        '[class*="banner" i]', '[class*="notice" i]',
        '[class*="cc-"]', '[id*="cc-"]',
    ].join(',');
    var containers = document.querySelectorAll(containerSel);
    for (var c = 0; c < containers.length; c++) {
        var btns = containers[c].querySelectorAll('button, [role="button"], a');
        for (var b = 0; b < btns.length; b++) {
            var btn = btns[b];
            if (btn.disabled) continue;
            var t = textOf(btn);
            if (t.length === 0 || t.length > 60) continue;
            if (hasWord(t, REJECT)) continue;
            if (hasWord(t, ACCEPT)) { btn.click(); return true; }
        }
    }
    return false;
})();
`

// dismissCookieBanner attempts to accept any cookie consent banner on the
// current page.  It loops up to three times with a short pause between
// iterations to handle two-step banners ("Manage preferences" → "Accept all").
// The loop stops as soon as no banner button is found.
func dismissCookieBanner(wd selenium.WebDriver) {
	for i := 0; i < 3; i++ {
		res, err := wd.ExecuteScript(jsAcceptCookies, nil)
		if err != nil || res != true {
			return
		}
		time.Sleep(250 * time.Millisecond) // wait for the overlay animation
	}
}

// jsAcceptDataConsent accepts a GDPR / data-processing consent gate.
// It differs from jsAcceptCookies in that it targets full-page or modal
// consent forms that gate the application form itself (not just cookie
// preference banners).  Strategy:
//  1. Tick every unchecked checkbox whose label / surrounding text mentions
//     data processing, privacy, GDPR, or application consent.
//  2. Click the first visible proceed / continue / submit button inside a
//     consent-scoped container.
//  3. If no scoped container is found, look for a standalone proceed button
//     whose text matches common consent-gate labels.
const jsAcceptDataConsent = `
(function(){
    var CONSENT_RE = /\b(privacy|data\s*(?:processing|protection|policy|consent)|gdpr|dsgvo|datenschutz|personal\s+data|application\s+terms|consent\s+to\s+(?:the\s*)?(?:processing|collection)|i\s+agree|i\s+accept|ich\s+stimme|ich\s+akzeptiere|bewerbungsdaten|einwilligung)\b/i;
    var PROCEED_WORDS = [
        'continue','proceed','next','weiter','submit','confirm','accept',
        'agree','ok','got it','ja','yes','fortfahren','bestätigen',
    ];

    function hasWord(text, words) {
        var t = text.toLowerCase().trim();
        for (var i = 0; i < words.length; i++) {
            if (t === words[i] || t.indexOf(words[i]) !== -1) return true;
        }
        return false;
    }
    function isVisible(el) {
        if (!el) return false;
        var s = window.getComputedStyle(el);
        if (s.display === 'none' || s.visibility === 'hidden' || parseFloat(s.opacity) < 0.1) return false;
        var r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
    }

    // Step 1: tick required consent checkboxes.
    var checked = 0;
    var cbs = document.querySelectorAll('input[type="checkbox"]');
    for (var i = 0; i < cbs.length; i++) {
        var cb = cbs[i];
        if (cb.checked || !isVisible(cb)) continue;
        // Check the label text and surrounding container text.
        var lbl = document.querySelector('label[for="'+cb.id+'"]');
        var lblTxt = lbl ? lbl.textContent : '';
        var parent = cb.parentElement;
        var ctx = lblTxt;
        for (var k = 0; k < 5 && parent; k++) { ctx += ' ' + (parent.textContent || ''); parent = parent.parentElement; }
        if (CONSENT_RE.test(ctx)) {
            cb.click();
            checked++;
        }
    }

    // Step 2: find a proceed/continue button in a consent-scoped container.
    var CONSENT_CONTAINERS = [
        // Jobvite GDPR consent form
        '.jv-gdpr', '[class*="gdpr"]', '[id*="gdpr"]',
        '[class*="consent"]', '[id*="consent"]',
        '[class*="privacy"]', '[id*="privacy"]',
        '[class*="datenschutz"]', '[id*="datenschutz"]',
        // Generic modal overlays that gate the form
        '[role="dialog"]', '.modal', '[class*="modal"]',
        '[class*="overlay"]', '[class*="gate"]',
    ];
    for (var ci = 0; ci < CONSENT_CONTAINERS.length; ci++) {
        var containers = document.querySelectorAll(CONSENT_CONTAINERS[ci]);
        for (var cj = 0; cj < containers.length; cj++) {
            var c = containers[cj];
            if (!isVisible(c)) continue;
            if (!CONSENT_RE.test(c.textContent)) continue;
            var btns = c.querySelectorAll('button, input[type="submit"], a[role="button"]');
            for (var bi = 0; bi < btns.length; bi++) {
                var b = btns[bi];
                if (!isVisible(b)) continue;
                if (hasWord(b.textContent || b.value || b.innerText || '', PROCEED_WORDS)) {
                    b.click();
                    return true;
                }
            }
        }
    }

    // Step 3: standalone proceed button not inside a labelled container —
    // only fire when we already ticked at least one consent checkbox (so we
    // know we are on a consent gate, not a normal form).
    if (checked > 0) {
        var allBtns = document.querySelectorAll('button[type="submit"], input[type="submit"], button');
        for (var ai = 0; ai < allBtns.length; ai++) {
            var ab = allBtns[ai];
            if (!isVisible(ab)) continue;
            if (hasWord(ab.textContent || ab.value || '', PROCEED_WORDS)) {
                ab.click();
                return true;
            }
        }
    }
    return checked > 0; // true if we at least ticked boxes (caller will wait for form)
})()
`

// dismissDataConsentWall detects and accepts GDPR / data-processing consent
// gates that block the application form on European ATS platforms (Jobvite,
// SmartRecruiters, and others).  It runs after clickPreApplyIfNeeded so the
// application form URL is already loaded.  On success it waits briefly for
// the real form to render; on failure it returns silently and lets the
// downstream form-ready check produce an appropriate error.
func dismissDataConsentWall(ctx context.Context, wd selenium.WebDriver) {
	res, err := wd.ExecuteScript(jsAcceptDataConsent, nil)
	if err != nil {
		return
	}
	accepted, _ := res.(bool)
	if !accepted {
		return
	}
	log.Printf("[consent] data-consent gate detected — accepted, waiting for form")
	// Wait up to 4 s for the form to appear after the consent gate dismisses.
	waitForElement(ctx, wd, appFormSelector, 4*time.Second)
}

// deadPagePhrases is a normalised list of substrings that appear in page
// titles, headings, or body text when a job posting is closed, filled,
// expired, or the URL 404s.  Ordered longest-first so the most specific
// phrase surfaces in error messages and shorter sub-strings don't shadow them.
var deadPagePhrases = []string{
	// Ashby-specific
	"this role is not currently open for applications",
	"this position is not currently open for applications",
	"not currently open for applications",
	"role is not currently open",
	"position is not currently open",
	"not currently accepting applications",
	"currently not accepting applications",
	"not open for applications",
	"applications are currently closed",
	"applications are closed",
	// Lever / Greenhouse
	"this job is no longer available",
	"this position is no longer available",
	"this role is no longer available",
	"this listing is no longer available",
	"this requisition is no longer available",
	"position has been filled",
	"this job has been filled",
	"this role has been filled",
	"this position has been filled",
	"job is no longer accepting",
	"no longer accepting applications",
	"this listing is no longer active",
	"this requisition is no longer active",
	"this posting is no longer active",
	// Expiry / removal
	"job posting has expired",
	"this job has expired",
	"this posting has expired",
	"this position has been removed",
	"this job has been removed",
	// Generic closed
	"this job has been closed",
	"this position is closed",
	"this job is closed",
	"job closed",
	"no longer available",
	// Generic 404 / error
	"page not found",
	"404 not found",
	"page doesn't exist",
	"page does not exist",
	"this page no longer exists",
	"we couldn't find that page",
	// German closed / expired
	"diese stelle ist nicht mehr verfügbar",
	"diese position ist nicht mehr verfügbar",
	"diese ausschreibung ist nicht mehr aktiv",
	"stelle wurde besetzt",
	"stelle ist besetzt",
	"bewerbung nicht mehr möglich",
	"stellenangebot nicht mehr verfügbar",
	"stelle abgelaufen",
	"stelle geschlossen",
	"seite nicht gefunden",
	"diese seite existiert nicht",
}

// jsDeadPage collects signals used by detectDeadPage in a single round-trip:
//   - HTTP response status (Navigation Timing API, Firefox 125+; 0 on older browsers)
//   - Lowercased page title
//   - Text from headings, prominent status/alert elements, paragraphs inside
//     <main> or <article>, and the first 2 000 chars of body text
//
// The 2 000-char window (up from 600) is important for React SPAs like Ashby
// where the "job closed" message is rendered deeper in the component tree.
const jsDeadPage = `
try {
    var status = 0;
    try {
        var nav = performance.getEntriesByType('navigation')[0];
        if (nav && nav.responseStatus) status = nav.responseStatus;
    } catch(e) {}

    var title = (document.title || '').toLowerCase();

    var parts = [];
    var selectors = [
        'h1','h2','h3','h4',
        '[role="alert"]','[role="status"]','[aria-live]',
        'main p','article p',
        '[class*="expired" i]','[class*="closed" i]',
        '[class*="not-found" i]','[class*="unavailable" i]',
        '[class*="empty-state" i]','[class*="empty_state" i]',
        '[id*="expired" i]','[id*="not-found" i]'
    ];
    selectors.forEach(function(s) {
        try {
            document.querySelectorAll(s).forEach(function(el) {
                var t = el.innerText || '';
                if (t.trim()) parts.push(t);
            });
        } catch(e) {}
    });
    parts.push((document.body && document.body.innerText || '').slice(0, 2000));
    var content = parts.join(' ').toLowerCase();

    return {status: status, title: title, content: content};
} catch(e) { return {status: 0, title: '', content: ''}; }
`

// detectDeadPage checks the current page for signs that a job posting no
// longer exists (HTTP 4xx/5xx, URL redirect to an error path, "job closed"
// headings, or known phrase patterns in the visible text).  Returns a
// descriptive error so the caller can skip this URL without wasting time on
// form-filling.
func detectDeadPage(wd selenium.WebDriver) error {
	// Fast URL check — some platforms redirect to /404, /not-found, /gone, etc.
	if u, err := wd.CurrentURL(); err == nil {
		uLow := strings.ToLower(u)
		for _, seg := range []string{"/404", "/not-found", "/error", "/gone", "/expired", "/closed"} {
			if strings.Contains(uLow, seg) {
				return fmt.Errorf("page redirected to error URL: %s", u)
			}
		}
	}

	// JS check: status + title + prominent-heading/body text in one round-trip.
	raw, err := wd.ExecuteScript(jsDeadPage, nil)
	if err != nil {
		return nil // can't tell — let the fill attempt proceed
	}
	data, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}

	// HTTP status (available on Firefox 125+; 0 on older browsers).
	if code, ok := data["status"].(float64); ok && code >= 400 {
		return fmt.Errorf("HTTP %.0f response", code)
	}

	title, _ := data["title"].(string)
	content, _ := data["content"].(string)
	combined := title + " " + content

	for _, phrase := range deadPagePhrases {
		if strings.Contains(combined, phrase) {
			return fmt.Errorf("job posting unavailable (%q detected on page)", phrase)
		}
	}

	return nil
}

// formReadySelector matches any visible text/email/tel input or textarea.
// Used as the post-fill-click wait target (broad enough to catch any activity).
const formReadySelector = `input[type="text"], input[type="email"], input[type="tel"], textarea`

// appFormSelector is tighter than formReadySelector: it only matches inputs
// that are characteristic of job application forms (email, first/last name).
// Used to decide whether an application form is present before trying to click
// an "Apply" button, so that search bars and cookie-consent inputs don't
// trigger a false "form already loaded" early exit.
// Also used as the post-click wait target so that a search bar already in the
// DOM (when Apply opens a modal overlay on the same page) doesn't cause the
// wait to return before the actual form fields have rendered.
const appFormSelector = `input[type="email"],` +
	`input[name*="email" i],` +
	`input[autocomplete="email"],` +
	`input[name*="first" i],` +
	`input[name*="last" i],` +
	`input[name="name"],` +
	`input[autocomplete="name"],` +
	`input[autocomplete="given-name"],` +
	`input[autocomplete="family-name"],` +
	`input[id*="email" i],` +
	`input[id*="firstname" i],` +
	`input[id*="first_name" i],` +
	// Workday uses data-automation-id instead of standard name/type/id.
	// firstName/lastName are unambiguous application-form fields (unlike "email"
	// which also appears on the Workday sign-in page).
	`[data-automation-id="firstName"],` +
	`[data-automation-id="lastName"]`

// clickPreApplyIfNeeded detects the "job description first, form second"
// pattern: if the page has no form inputs yet it looks for an "Apply" trigger
// button, clicks it, then waits up to 10 s for the form to appear.
// Returns true when it performed a click (regardless of whether the form
// appeared — the caller's waitForElement inside the fill function will handle
// the remaining wait).
func clickPreApplyIfNeeded(ctx context.Context, wd selenium.WebDriver) bool {
	// Skip the Apply-button hunt only when application-form inputs are already
	// visible INSIDE THE VIEWPORT — not merely present in the DOM.
	//
	// offsetParent !== null is NOT sufficient: platforms like job-boards.greenhouse.io
	// render the full form below the job description on the same page. Those inputs
	// have a non-null offsetParent (they're in the normal document flow, just off-
	// screen), which caused the old check to bail out immediately and never click
	// the "Apply for this job" CTA at the top of the page — leaving Simplify
	// invoked without the form having been activated.
	//
	// getBoundingClientRect() measures position relative to the viewport, so an
	// input below the fold returns top >= window.innerHeight and is correctly
	// treated as not-yet-visible, triggering the Apply-button click.
	res, _ := wd.ExecuteScript(`
var els = document.querySelectorAll(arguments[0]);
for (var i = 0; i < els.length; i++) {
    var e = els[i];
    if (e.disabled) continue;
    var r = e.getBoundingClientRect();
    if (r.width > 0 && r.height > 0 && r.top < window.innerHeight && r.bottom > 0) return true;
}
return false;`, []interface{}{appFormSelector})
	if res == true {
		return false
	}

	clicked := false

	// 1. Attribute-based CSS — highest precision, platform-specific IDs/classes.
	// Ordered: ATS-specific first, then generic data attributes.
	for _, sel := range []string{
		// BambooHR
		`a[class*="BambooHR-ATS-Jobs-Apply"]`,
		`[class*="BambooHR-ATS-Jobs-Apply"]`,
		// Greenhouse, Lever, Ashby, generic ATS
		`button[data-qa*="apply" i]`, `a[data-qa*="apply" i]`,
		`button[id*="apply-btn" i]`, `button[id*="btn-apply" i]`,
		`a[id*="apply-btn" i]`, `a[id*="btn-apply" i]`,
		`button[class*="apply-btn" i]`, `a[class*="apply-btn" i]`,
		`a[class*="jobs-apply" i]`, `button[class*="jobs-apply" i]`,
		`[data-automation*="apply" i]`,
		`button[data-testid*="apply" i]`, `a[data-testid*="apply" i]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		// Try every match — the first one in DOM order may be off-screen.
		for _, el := range els {
			if el.Click() == nil {
				clicked = true
				break
			}
		}
		if clicked {
			break
		}
	}

	// 2. XPath text — longest phrases first to avoid matching sub-strings in
	// unrelated elements (e.g. "Browse and apply" matching "apply").
	if !clicked {
		const lc = `translate(normalize-space(.),'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz')`
		for _, phrase := range []string{
			// English
			"apply to this job",
			"apply for this position",
			"apply for this role",
			"apply for this job",
			"start your application",
			"start application",
			"begin application",
			"apply with linkedin",
			"apply with resume",
			"quick apply",
			"1-click apply",
			"apply now",
			"easy apply",
			// German
			"jetzt bewerben",
			"zur bewerbung",
			"bewerbung starten",
			"bewerben sie sich",
			"hier bewerben",
			"online bewerben",
		} {
			xpath := fmt.Sprintf(`(//button|//a)[contains(%s,'%s')]`, lc, phrase)
			els, err := wd.FindElements(selenium.ByXPATH, xpath)
			if err != nil || len(els) == 0 {
				continue
			}
			for _, el := range els {
				if el.Click() == nil {
					clicked = true
					break
				}
			}
			if clicked {
				break
			}
		}
		// Bare "apply" only when the entire label is exactly that word, to
		// prevent matching navigation links like "Jobs / Apply" breadcrumbs.
		if !clicked {
			for _, bare := range []string{"apply", "bewerben"} {
				xpath := fmt.Sprintf(`(//button|//a)[%s='%s']`, lc, bare)
				els, err := wd.FindElements(selenium.ByXPATH, xpath)
				if err == nil {
					for _, el := range els {
						if el.Click() == nil {
							clicked = true
							break
						}
					}
				}
				if clicked {
					break
				}
			}
		}
	}

	// 3. JS fallback — scrolls each candidate into view before clicking so
	// off-screen buttons (sticky headers, bottom CTAs) are reachable.
	// Excludes nav/footer/[role="navigation"] but NOT <header> — legitimate
	// ATS apply buttons are frequently placed inside sticky page headers.
	if !clicked {
		const jsApply = `
var MULTI = /\b(apply\s+(?:to\s+this\s+(?:job|position|role)|for\s+this\s+(?:job|position|role)|now|with\s+\S+)|easy\s+apply|quick\s+apply|(?:start|begin)\s+(?:your\s+)?application|1[\s-]click\s+apply|jetzt\s+bewerben|zur\s+bewerbung|bewerbung\s+starten|bewerben\s+sie\s+sich|hier\s+bewerben|online\s+bewerben)\b/i;
var BARE  = /^\s*(?:apply|bewerben)\s*$/i;
var all = Array.from(document.querySelectorAll('button, a[href], [role="button"]'));
for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (el.disabled || el.offsetParent === null) continue;
    if (el.closest('nav, footer, [role="navigation"]')) continue;
    var t = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
    if (MULTI.test(t) || BARE.test(t)) {
        el.scrollIntoView({block: 'center'});
        el.click();
        return true;
    }
}
return false;`
		res, err := wd.ExecuteScript(jsApply, nil)
		if err == nil && res == true {
			clicked = true
		}
	}

	if clicked {
		// Wait for application-specific inputs (email / name fields) rather than
		// any text input.  When Apply opens a modal overlay the underlying page
		// may already have a search bar that would satisfy formReadySelector
		// immediately, before the modal's own fields have rendered.
		waitForElement(ctx, wd, appFormSelector, 12*time.Second)
	}
	return clicked
}

// jsFill scrolls the first element matching a CSS selector into view, then
// sets its value via the native HTMLInputElement/HTMLTextAreaElement prototype
// setter and fires input/change/blur so React, Vue, and Angular frameworks
// pick up the new value — plain SendKeys does not reliably trigger these.
// Returns true when an element was found and filled, false otherwise so the
// Go caller can distinguish "filled" from "no visible element found".
const jsFill = `
var sel = arguments[0], val = arguments[1];
var els = document.querySelectorAll(sel), el = null;
for (var i = 0; i < els.length; i++) {
    var e = els[i];
    if (e.disabled) continue;
    // offsetParent is null for:
    //   a) elements whose ancestor has display:none  — truly hidden, skip
    //   b) elements with position:fixed              — visible, keep
    // Check computed position to distinguish the two.
    if (e.offsetParent !== null || window.getComputedStyle(e).position === 'fixed') {
        el = e; break;
    }
}
if (!el) return false;
el.focus();
el.scrollIntoView({block: 'center'});
try {
    var proto = el.tagName === 'TEXTAREA'
        ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
    setter.call(el, val);
} catch (e) { el.value = val; }
el.dispatchEvent(new Event('input',  {bubbles: true}));
el.dispatchEvent(new Event('change', {bubbles: true}));
el.dispatchEvent(new Event('blur',   {bubbles: true}));
return true;
`

// jsFillByLabel finds the first visible input associated with a <label> whose
// text contains the given needle (case-insensitive), fills it with val using
// the same native-setter approach as jsFill, and returns true on success.
// This is the most reliable fallback for React/Vue forms that expose no
// standard name/id/placeholder attributes but do have visible labels.
const jsFillByLabel = `
var needle = arguments[0].toLowerCase(), val = arguments[1];
var labels = document.querySelectorAll('label');
for (var i = 0; i < labels.length; i++) {
    var l = labels[i];
    if (l.textContent.trim().toLowerCase().indexOf(needle) === -1) continue;
    var inp = null;
    // 1. Standard HTML: label[for] → getElementById
    var fid = l.getAttribute('for');
    if (fid) inp = document.getElementById(fid);
    // 2. Input nested inside the label element
    if (!inp) inp = l.querySelector('input:not([type="hidden"]):not([type="checkbox"]):not([type="radio"]), textarea');
    // 3. Sibling pattern (BambooHR, many modern forms): label and input share a
    //    parent container but are not linked via for/id.  Walk following siblings
    //    until we find an input or hit the next label (= next field boundary).
    if (!inp) {
        var sib = l.nextElementSibling;
        while (sib) {
            if (sib.tagName === 'LABEL') break;
            if ((sib.tagName === 'INPUT' || sib.tagName === 'TEXTAREA')
                    && sib.type !== 'hidden' && sib.type !== 'checkbox' && sib.type !== 'radio') {
                inp = sib; break;
            }
            var inner = sib.querySelector('input:not([type="hidden"]):not([type="checkbox"]):not([type="radio"]), textarea');
            if (inner) { inp = inner; break; }
            sib = sib.nextElementSibling;
        }
    }
    if (!inp || inp.disabled) continue;
    // Same visibility logic as jsFill: accept position:fixed elements.
    if (inp.offsetParent === null && window.getComputedStyle(inp).position !== 'fixed') continue;
    inp.focus();
    inp.scrollIntoView({block: 'center'});
    try {
        var proto = inp.tagName === 'TEXTAREA'
            ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
        var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
        setter.call(inp, val);
    } catch(e) { inp.value = val; }
    inp.dispatchEvent(new Event('input',  {bubbles: true}));
    inp.dispatchEvent(new Event('change', {bubbles: true}));
    inp.dispatchEvent(new Event('blur',   {bubbles: true}));
    return true;
}
return false;
`

// tryFillByLabel fills the input whose label text contains labelText.
// Returns true when an element was found and filled.
func tryFillByLabel(wd selenium.WebDriver, labelText, value string) bool {
	if value == "" {
		return false
	}
	res, err := wd.ExecuteScript(jsFillByLabel, []interface{}{labelText, value})
	return err == nil && res == true
}

// jsSubmit finds and clicks the most likely submit button via JS heuristics,
// returning true when it clicked something.  Used as a last resort after all
// CSS/XPath attempts fail.
const jsSubmit = `
var WORDS = /\b(submit|apply|send|complete|finish|confirm)\b/i;
function vis(el){
    if(el.disabled) return false;
    var r=el.getBoundingClientRect();
    return r.width>0 && r.height>0 && r.bottom>0 && r.top<window.innerHeight;
}
var all = Array.from(document.querySelectorAll(
    'button[type="submit"], input[type="submit"], button, [role="button"]'
));
for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (!vis(el)) continue;
    var label = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
    if (el.type === 'submit' || WORDS.test(label)) {
        el.scrollIntoView({block: 'center'});
        el.click();
        return true;
    }
}
return false;
`

// waitForElement polls for selector to match at least one element, returning
// the first match or nil after timeout.  Requires implicit wait to be 0 so
// each FindElements call returns immediately.
func waitForElement(ctx context.Context, wd selenium.WebDriver, selector string, timeout time.Duration) selenium.WebElement {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		els, err := wd.FindElements(selenium.ByCSSSelector, selector)
		if err == nil && len(els) > 0 {
			return els[0]
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// trySetInput fills the first visible element matching selector using the JS
// native-setter approach (required for React/Vue controlled inputs) and returns
// true only when an element was found AND the value was actually written.
// If jsFill cannot find a usable visible element it returns false so callers
// can continue down the fallback chain rather than silently stopping.
// As a last resort it tries WebDriver SendKeys on every matched element.
func trySetInput(wd selenium.WebDriver, selector, value string) bool {
	if value == "" {
		return false
	}
	els, err := wd.FindElements(selenium.ByCSSSelector, selector)
	if err != nil || len(els) == 0 {
		return false
	}
	res, err := wd.ExecuteScript(jsFill, []interface{}{selector, value})
	if err == nil && res == true {
		return true
	}
	// jsFill found no usable visible element (returned false) or JS errored.
	// Fall back to WebDriver SendKeys on every matched element — works for
	// plain-HTML forms that don't need synthetic events.
	for _, el := range els {
		_ = el.Click()
		_ = el.Clear()
		if err2 := el.SendKeys(value); err2 == nil {
			return true
		}
	}
	return false
}

// uploadSelectors are tried in order by uploadResume.
// Ordered from most specific (known attribute values) to most generic.
var uploadSelectors = []string{
	`input[name="resume"]`,
	`input[name="cv"]`,
	`input[name*="resume" i]`,
	`input[name*="cv" i]`,
	`input[name*="attachment" i]`,
	`input[accept*=".pdf" i]`,
	`input[accept*="pdf" i]`,
	`input[type="file"]`,
}

// jsRevealFileInputs makes every file input in the document reachable via
// SendKeys.  Many ATS platforms hide the real <input type="file"> behind a
// styled drag-drop zone or a custom button — the CSS hiding (display:none,
// opacity:0, pointer-events:none, etc.) prevents geckodriver from injecting
// the file path.  We force all of them into a minimal 1×1 px visible rect
// before attempting the upload, then rely on the browser to handle the rest.
const jsRevealFileInputs = `
var inputs = document.querySelectorAll('input[type="file"]');
for (var i = 0; i < inputs.length; i++) {
    inputs[i].style.cssText = [
        'display:block!important',
        'opacity:1!important',
        'visibility:visible!important',
        'position:fixed!important',
        'left:0px!important',
        'top:0px!important',
        'width:1px!important',
        'height:1px!important',
        'overflow:visible!important',
        'pointer-events:auto!important'
    ].join(';');
}
return inputs.length;
`

// uploadResume tries to upload path to any file input on the current page.
// Pass 1: reveals hidden file inputs in the main frame and tries all selectors.
// Pass 2: switches into each iframe and repeats — iCIMS and some other ATS
// platforms host the real file input inside a sandboxed frame.
func uploadResume(wd selenium.WebDriver, path string) bool {
	if path == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		log.Printf("[upload] cannot resolve path %q: %v", path, err)
		return false
	}
	if _, err := os.Stat(absPath); err != nil {
		log.Printf("[upload] resume file not found: %q", absPath)
		return false
	}

	base := filepath.Base(absPath)

	// tryInCurrentFrame reveals hidden file inputs then walks uploadSelectors.
	tryInCurrentFrame := func() bool {
		if n, err2 := wd.ExecuteScript(jsRevealFileInputs, nil); err2 == nil {
			if cnt, ok := n.(float64); ok && cnt > 0 {
				time.Sleep(150 * time.Millisecond)
			}
		}
		for _, sel := range uploadSelectors {
			els, ferr := wd.FindElements(selenium.ByCSSSelector, sel)
			if ferr != nil || len(els) == 0 {
				continue
			}
			for _, el := range els {
				if kerr := el.SendKeys(absPath); kerr != nil {
					log.Printf("[upload] SendKeys failed on %q: %v", sel, kerr)
					continue
				}
				log.Printf("[upload] %q uploaded via selector %q", base, sel)
				return true
			}
		}
		return false
	}

	// Pass 1: main frame.
	if tryInCurrentFrame() {
		return true
	}

	// Pass 2: iframes (iCIMS and others embed the file input inside a frame).
	iframes, _ := wd.FindElements(selenium.ByCSSSelector, "iframe")
	for _, iframe := range iframes {
		if ferr := wd.SwitchFrame(iframe); ferr != nil {
			continue
		}
		found := tryInCurrentFrame()
		_ = wd.SwitchFrame(nil) // return to main frame
		if found {
			return true
		}
	}

	log.Printf("[upload] WARNING: no file input found on page — resume not uploaded")
	return false
}

// submitWithAshbyRetry clicks submit on an Ashby form, reads the validation
// error banner that appears on failure, attempts targeted fixes for each
// error, and retries — up to maxAshbyRetries additional times.
//
// Ashby validation errors contain the field label (e.g. "how did you hear
// about us", "are you legally authorized to work in the united states?").
// We match each error string against known fix patterns and call the
// appropriate filler before re-attempting submission.
func submitWithAshbyRetry(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, flags FillFlags) error {
	const maxAshbyRetries = 2

	authAns := info.WorkAuthorized
	if authAns == "" {
		authAns = "yes"
	}
	sponsorAns := info.RequireSponsorship
	if sponsorAns == "" {
		sponsorAns = "no"
	}

	for attempt := 0; attempt <= maxAshbyRetries; attempt++ {
		// Re-accept consent checkboxes and scroll to bottom on every attempt.
		// Ashby renders consent/privacy checkboxes at the very bottom of the form;
		// they may only appear after other fields are filled, so we re-run here
		// rather than relying solely on the earlier fillCommonExtras call.
		_, _ = wd.ExecuteScript(jsAcceptPrivacyConsent, nil)
		_, _ = wd.ExecuteScript(`window.scrollTo(0, document.body.scrollHeight);`, nil)
		time.Sleep(400 * time.Millisecond)
		_, _ = wd.ExecuteScript(jsAcceptPrivacyConsent, nil) // second pass after scroll reveals bottom

		if err := clickSubmit(wd); err != nil {
			return err
		}
		clickConfirmationModal(wd)

		// Give React time to process the submit event and render any errors.
		time.Sleep(1000 * time.Millisecond)

		// Read validation errors from the Ashby error summary banner.
		raw, err := wd.ExecuteScript(jsReadAshbyValidationErrors, nil)
		if err != nil || raw == nil {
			// Can't read errors — fall through to standard verify.
			break
		}
		errItems, ok := raw.([]interface{})
		if !ok || len(errItems) == 0 {
			// No error banner — submission either succeeded or Ashby gave no signal.
			break
		}

		if attempt == maxAshbyRetries {
			// Exhausted retries — log remaining errors and let verifySubmission decide.
			for _, e := range errItems {
				log.Printf("[ashby] validation error (giving up): %v", e)
			}
			break
		}

		log.Printf("[ashby] %d validation error(s) after submit attempt %d — attempting fixes", len(errItems), attempt+1)

		for _, ev := range errItems {
			msg, _ := ev.(string)
			if msg == "" {
				continue
			}
			log.Printf("[ashby] error: %s", msg)

			switch {
			case contains(msg, "how did you hear", "hear about us", "source", "referral"):
				// MultiValueSelect — open and pick first option.
				wd.ExecuteScript(jsClickFirstMultiValueOption, []interface{}{"how did you hear"}) //nolint:errcheck
				time.Sleep(1 * time.Second)
				// Close dropdown by pressing Escape if still open.
				if inps, e := wd.FindElements(selenium.ByCSSSelector, `[aria-expanded="true"]`); e == nil {
					for _, inp := range inps {
						inp.SendKeys(selenium.EscapeKey) //nolint:errcheck
					}
				}

			case contains(msg, "authorized to work", "legally authorized", "work authorization", "eligible to work"):
				wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"authorized to work", authAns})        //nolint:errcheck
				wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"legally authorized to work", authAns}) //nolint:errcheck
				fillComboboxByLabel(wd, "authorized to work", authAns)
				fillComboboxByLabel(wd, "legally authorized to work", authAns)
				fillComboboxByLabel(wd, "eligible to work", authAns)

			case contains(msg, "sponsorship", "visa", "require a visa"):
				wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{"require sponsorship", sponsorAns}) //nolint:errcheck
				fillComboboxByLabel(wd, "require sponsorship", sponsorAns)
				fillComboboxByLabel(wd, "require a visa", sponsorAns)

			case contains(msg, "state would you like", "state based", "location", "where are you based"):
				if !fillComboboxByLabel(wd, "state would you like to be based", info.City) {
					if !fillComboboxByLabel(wd, "state would you like to be based", "remote") {
						fillComboboxByLabel(wd, "state would you like to be based", "other")
					}
				}
				fillComboboxByLabel(wd, "location are you applying for", "other")

			case contains(msg, "right to work", "nature of your right"):
				fillComboboxByLabel(wd, "nature of your right to work", "unlimited")
				fillComboboxByLabel(wd, "right to work", authAns)

			case contains(msg, "gender", "ethnicity", "race", "disability", "veteran"):
				wd.ExecuteScript(jsHandleEEORadios, []interface{}{"decline", "decline"}) //nolint:errcheck

			case contains(msg, "language", "german", "english proficiency", "deutsch"):
				for _, lbl := range []string{"german language", "level of german", "german proficiency", "deutsch", "english language", "level of english"} {
					if fillComboboxByLabel(wd, lbl, "fluent") {
						break
					}
					fillComboboxByLabel(wd, lbl, "native")
				}

			case contains(msg, "based in", "located in", "reside in", "currently in"):
				for _, lbl := range []string{
					"currently based in germany", "currently located in germany",
					"currently based in europe", "currently located in europe",
					"currently based in the eu", "reside in germany",
				} {
					fillComboboxByLabel(wd, lbl, "yes")
					wd.ExecuteScript(jsHandleAshbyYesNo, []interface{}{lbl, "yes"}) //nolint:errcheck
				}

			default:
				// Unknown field — run the generic mop-up, then also try to fill
				// any remaining Ashby comboboxes with their first available option.
				_, ashbyLocalPhone := splitPhone(info.Phone)
				wd.ExecuteScript(jsFillRequiredFields, []interface{}{ //nolint:errcheck
					info.CoverLetter, info.ExpectedSalary, info.Phone, info.Website, ashbyLocalPhone,
				})
				fillAshbyUnfilledComboboxes(wd)
			}
		}

		// Short pause for React to re-evaluate form validity after fixes.
		time.Sleep(500 * time.Millisecond)
	}

	return verifySubmission(ctx, wd, flags.Headful || flags.Hold)
}

// contains reports whether s contains any of the substrings (case already
// lowercased by the caller).
func contains(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// jsVerifySubmission checks whether the page shows signs of a successful
// submission or a validation/server error, collecting all signals in one
// round-trip.  Returns:
//   {success: true,  phrase: "..."}  — confirmation phrase detected
//   {success: false, error: "..."}   — visible form-validation error element
//   {success: false, serverError: "..."} — server-side failure phrase detected
//   {success: false}                 — no conclusive signal
const jsVerifySubmission = `
try {
    var txt   = ((document.body && document.body.innerText) || ‘’).toLowerCase();
    var title = (document.title || ‘’).toLowerCase();
    var combined = title + ‘ ‘ + txt.slice(0, 3000);

    var ok = [
        ‘thank you for applying’,’thanks for applying’,
        ‘application received’,’application submitted’,
        ‘successfully submitted’,’successfully applied’,
        ‘we will review’,’we’ll be in touch’,"we’ll be in touch",
        ‘you have applied’,’application complete’,
        ‘your application has been’,’application was submitted’,
        ‘submission confirmed’,’we received your application’,
        ‘your application is complete’,’application is under review’,
        // Greenhouse / Lever specific
        ‘thanks for applying’,’your application has been submitted’,
        ‘you have successfully applied’,’application sent’,
        // Workable / generic
        ‘your application was sent’,’we got your application’,
        ‘application acknowledged’,’congrats’,’you applied’,
        ‘we have received’,’thank you for your interest’,
        // German ATS
        ‘bewerbung erhalten’,’bewerbung eingegangen’,
        ‘vielen dank für ihre bewerbung’,’bewerbung erfolgreich’,
        // Ashby duplicate / already-submitted
        ‘you have already applied’,’already applied to this’,
        ‘you already applied’,’your application has already been’,
        ‘already submitted an application’
    ];
    for (var i = 0; i < ok.length; i++) {
        if (combined.indexOf(ok[i]) !== -1) return {success: true, phrase: ok[i]};
    }

    // Server-side failure phrases — distinct from per-field validation errors.
    var fail = [
        ‘submission failed’,’failed to submit’,’error submitting’,
        ‘unable to submit’,’could not submit’,
        ‘something went wrong’,’an error occurred’,
        ‘please try again’,’try again later’,
        ‘submission unsuccessful’,’unable to process your application’,
        ‘we encountered a problem’,’there was a problem’,
        ‘there was an error’,’application could not be submitted’,
        ‘your application was not submitted’
    ];
    for (var k = 0; k < fail.length; k++) {
        if (combined.indexOf(fail[k]) !== -1) return {success: false, serverError: fail[k]};
    }

    // Visible validation-error elements — indicates the form is still open with errors.
    var errSels = ‘[class*="error" i]:not([class*="error-page" i]),[class*="invalid" i],[aria-invalid="true"],[data-error],.field-error,.form-error’;
    var errEls  = document.querySelectorAll(errSels);
    for (var j = 0; j < errEls.length; j++) {
        var t = (errEls[j].innerText || ‘’).trim();
        if (t && t.length < 300) return {success: false, error: t};
    }
    return {success: false};
} catch(e) { return {success: false}; }
`

// successURLSegs are path / query segments that indicate a thank-you or
// confirmation page after an ATS form submission.
var successURLSegs = []string{
	"thank", "thanks", "success", "confirm", "submitted",
	"complete", "done", "received", "applied",
	"/ty", "apply/ty", // Lever thank-you redirect
}

// errorURLSegs are path segments that indicate an error or failure page after
// a form submission redirect.
var errorURLSegs = []string{"/error", "/failed", "/failure", "/problem", "/oops"}

// verifySubmission waits up to 15 s after submit for a confirmation signal:
// a URL redirect to a thank-you page, a success phrase in the page text, or
// absence of validation errors.  On headful mode it keeps the window open an
// extra 15 s when a form error is detected so the user can see what went wrong.
// Returns nil when the submission appears successful or when no signal can be
// detected (some ATS platforms give no visible feedback).
func verifySubmission(ctx context.Context, wd selenium.WebDriver, headful bool) error {
	originalURL, _ := wd.CurrentURL()
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}

		// URL change is the strongest signal — most ATS platforms redirect to a
		// thank-you or confirmation page after successful submission.
		if cur, _ := wd.CurrentURL(); cur != originalURL {
			low := strings.ToLower(cur)

			// Explicit error path segments beat everything else.
			for _, seg := range errorURLSegs {
				if strings.Contains(low, seg) {
					return fmt.Errorf("submission redirected to error page: %s", cur)
				}
			}

			for _, seg := range successURLSegs {
				if strings.Contains(low, seg) {
					log.Printf("[apply] submission confirmed via redirect: %s", cur)
					return nil
				}
			}

			// Ambiguous redirect — inspect the new page before declaring success.
			if raw, err := wd.ExecuteScript(jsVerifySubmission, nil); err == nil {
				if data, ok := raw.(map[string]interface{}); ok {
					if svrErr, _ := data["serverError"].(string); svrErr != "" {
						if headful {
							log.Printf("[apply] server error on redirect — keeping window open 15 s: %s", svrErr)
							time.Sleep(15 * time.Second)
						}
						return fmt.Errorf("submission failed: %s", svrErr)
					}
					if errMsg, _ := data["error"].(string); errMsg != "" {
						if headful {
							log.Printf("[apply] form validation error on redirect — keeping window open 15 s: %s", errMsg)
							time.Sleep(15 * time.Second)
						}
						return fmt.Errorf("form validation failed: %s", errMsg)
					}
				}
			}

			// No error signals on the new page — treat navigation as acceptance.
			log.Printf("[apply] form navigated → %s (treating as submitted)", cur)
			return nil
		}

		// Page-content check: success phrase, server error, or validation error.
		raw, err := wd.ExecuteScript(jsVerifySubmission, nil)
		if err != nil {
			continue
		}
		data, _ := raw.(map[string]interface{})
		if data["success"] == true {
			phrase, _ := data["phrase"].(string)
			log.Printf("[apply] submission confirmed (%q)", phrase)
			return nil
		}
		if svrErr, _ := data["serverError"].(string); svrErr != "" {
			if headful {
				log.Printf("[apply] server error — keeping window open 15 s: %s", svrErr)
				time.Sleep(15 * time.Second)
			}
			return fmt.Errorf("submission failed: %s", svrErr)
		}
		if errMsg, _ := data["error"].(string); errMsg != "" {
			if headful {
				log.Printf("[apply] form validation error — keeping window open 15 s: %s", errMsg)
				time.Sleep(15 * time.Second)
			}
			return fmt.Errorf("form validation failed: %s", errMsg)
		}
	}

	// No signal in 10 s — warn and proceed; many ATS platforms are silent.
	log.Printf("[apply] warning: no submission confirmation signal detected")
	return nil
}

// clickConfirmationModal handles "are you sure?" dialogs that some ATS platforms
// (most notably Greenhouse) display after the first Submit click.  Greenhouse
// shows a #application_confirm modal with an id="application_confirm" button.
// The new job-boards.greenhouse.io format uses a <dialog> element or an
// aria-modal container instead of the legacy role="dialog" div.
// This runs after clickSubmit returns so the 2-second sleep has already passed.
func clickConfirmationModal(wd selenium.WebDriver) {
	// Allow up to 5 s — new Greenhouse React format may take longer to mount the dialog.
	const maxWait = 5 * time.Second
	const poll = 300 * time.Millisecond
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		for _, sel := range []string{
			// Old Greenhouse boards format
			`#application_confirm`,
			`button[id*="confirm" i]`,
			// New Greenhouse job-boards format uses <dialog> element
			`dialog button[type="submit"]`,
			`dialog button[type="button"]`,
			`dialog button`,
			// aria-modal containers (React portals)
			`[aria-modal="true"] button[type="submit"]`,
			`[aria-modal="true"] button[type="button"]`,
			`[aria-modal="true"] button`,
			// role=dialog / alertdialog (legacy + generic)
			`[role="dialog"] button[type="submit"]`,
			`[role="dialog"] button[type="button"]`,
			`[role="alertdialog"] button`,
			`.modal button[type="submit"]`,
			`.modal-dialog button[type="submit"]`,
		} {
			els, err := wd.FindElements(selenium.ByCSSSelector, sel)
			if err != nil || len(els) == 0 {
				continue
			}
			for _, el := range els {
				text, _ := el.Text()
				tl := strings.ToLower(strings.TrimSpace(text))
				// Only click buttons whose label suggests confirmation.
				if tl == "" || strings.Contains(tl, "submit") || strings.Contains(tl, "confirm") ||
					strings.Contains(tl, "yes") || strings.Contains(tl, "ok") || strings.Contains(tl, "apply") {
					if el.Click() == nil {
						log.Printf("[apply] confirmation dialog clicked (%q)", tl)
						time.Sleep(1500 * time.Millisecond)
						return
					}
				}
			}
		}
		time.Sleep(poll)
	}
}

// submitButtonTexts covers the button labels used across ATS platforms.
// Ordered from most specific to most generic to avoid false positives.
var submitButtonTexts = []string{
	"submit application", "submit my application",
	"complete application", "complete my application",
	"send application", "send my application",
	"apply now", "apply for this job", "apply for this position",
	"submit", "apply", "send", "complete", "finish", "confirm",
}

// clickSubmit tries CSS attribute selectors, XPath text search across all
// common ATS button labels, and finally a JS heuristic as a last resort.
func clickSubmit(wd selenium.WebDriver) error {
	// 1. Attribute-based CSS — most reliable; not text-dependent.
	for _, sel := range []string{
		`button[type="submit"]`,
		`input[type="submit"]`,
		`button[data-qa*="submit" i]`,
		`button[data-testid*="submit" i]`,
		`button[data-testid*="apply" i]`,
		`button[aria-label*="submit" i]`,
		`button[aria-label*="apply" i]`,
		`[role="button"][aria-label*="submit" i]`,
		`[role="button"][aria-label*="apply" i]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			_, _ = wd.ExecuteScript(`arguments[0].scrollIntoView({block:'center'})`, []interface{}{els[0]})
			if cerr := els[0].Click(); cerr == nil {
				time.Sleep(1500 * time.Millisecond)
				return nil
			}
		}
	}

	// 2. XPath text search — covers buttons labelled with human-readable text.
	const lowerXPath = `translate(normalize-space(.),'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz')`
	for _, phrase := range submitButtonTexts {
		xpath := fmt.Sprintf(`//button[contains(%s,'%s')] | //a[contains(%s,'%s')]`,
			lowerXPath, phrase, lowerXPath, phrase)
		els, err := wd.FindElements(selenium.ByXPATH, xpath)
		if err == nil && len(els) > 0 {
			_, _ = wd.ExecuteScript(`arguments[0].scrollIntoView({block:'center'})`, []interface{}{els[0]})
			if cerr := els[0].Click(); cerr == nil {
				time.Sleep(1500 * time.Millisecond)
				return nil
			}
		}
	}

	// 3. JS heuristic — skips disabled buttons (prefers enabled ones).
	res, err := wd.ExecuteScript(jsSubmit, nil)
	if err == nil && res == true {
		time.Sleep(1500 * time.Millisecond)
		return nil
	}

	// 4. Force-dispatch a synthetic click event on the submit button even if
	// it has the disabled attribute.  The mop-up pass should have enabled it,
	// but React sometimes needs the click event to re-evaluate form state.
	log.Printf("[apply] normal click failed — dispatching synthetic click on submit button")
	res2, err2 := wd.ExecuteScript(jsForceClickSubmit, nil)
	if err2 == nil && res2 == true {
		time.Sleep(1500 * time.Millisecond)
		return nil
	}

	return fmt.Errorf("no submit button found on page")
}

// jsClickSimplify finds and clicks the Simplify extension's injected autofill
// button.  We fire a full synthetic mouse-event sequence (mouseover → mousedown
// → mouseup → click) because React / Vue components often ignore a bare .click()
// call that bypasses their synthetic event system.  Four strategies are tried in
// order of specificity:
//
//  1. Elements whose class/id/data attribute contains "simplify" that are, or
//     contain, a visible button or role=button element.
//  2. Text match against known Simplify autofill label variants.
//  3. Any visible button inside a fixed- or sticky-positioned ancestor whose
//     text contains "simplify" or "autofill".
//  4. Shadow-DOM pierce: walk every shadow root reachable from document.body
//     and repeat strategies 1-3 inside each.
const jsClickSimplify = `
(function(){
    // Strategy 0: target the known Simplify shadow host directly.
    // The extension injects <div class="simplify-jobs-shadow-root"> with an
    // open shadow root — check it first before full DOM traversal.
    var _host=document.querySelector('.simplify-jobs-shadow-root');
    if(_host&&_host.shadowRoot){
        var _btns=_host.shadowRoot.querySelectorAll('button,[role="button"]');
        for(var _i=0;_i<_btns.length;_i++){
            var _t=(_btns[_i].innerText||_btns[_i].textContent||_btns[_i].getAttribute('aria-label')||'').trim();
            if(/autofill|simplify/i.test(_t)){
                _btns[_i].scrollIntoView({block:'center'});
                _btns[_i].click();
                return true;
            }
        }
    }

    function vis(el){
        if(!el) return false;
        var s=window.getComputedStyle(el);
        return s.display!=='none' && s.visibility!=='hidden' && s.opacity!=='0' &&
               (el.offsetParent!==null || s.position==='fixed' || s.position==='sticky');
    }
    function fire(el){
        if(!el||el.disabled||!vis(el)) return false;
        el.scrollIntoView({block:'center',inline:'nearest'});
        ['mouseover','mousedown','mouseup','click'].forEach(function(t){
            el.dispatchEvent(new MouseEvent(t,{bubbles:true,cancelable:true,view:window}));
        });
        el.click();
        return true;
    }
    function tryRoot(root){
        // Strategy 1: simplify class/id/data attributes
        var sel1=
            'button[class*="simplify" i],button[id*="simplify" i],' +
            '[class*="simplify" i] button,[id*="simplify" i] button,' +
            '[class*="simplify" i][role="button"],[id*="simplify" i][role="button"],' +
            '[data-simplify] button,[data-extension="simplify"] button,' +
            '[class*="sj-"] button,[id*="sj-"] button';
        var s1=root.querySelectorAll(sel1);
        for(var i=0;i<s1.length;i++){ if(fire(s1[i])) return true; }

        // Strategy 2: known Simplify autofill label text
        var FILL=/^\s*(autofill|fill\s+application|fill\s+form|apply\s+with\s+simplify|simplify\s+autofill|autofill\s+application|auto-?fill)\s*$/i;
        var btns=root.querySelectorAll('button,[role="button"]');
        for(var j=0;j<btns.length;j++){
            var t=(btns[j].innerText||btns[j].textContent||btns[j].getAttribute('aria-label')||'').trim();
            if(FILL.test(t) && fire(btns[j])) return true;
        }

        // Strategy 3: any button in a fixed/sticky ancestor with "simplify" or "autofill" text
        var BROAD=/simplify|autofill/i;
        for(var k=0;k<btns.length;k++){
            var btn=btns[k];
            if(!vis(btn)) continue;
            var p=btn.parentElement,inFixed=false;
            while(p){ var ps=window.getComputedStyle(p).position;
                if(ps==='fixed'||ps==='sticky'){inFixed=true;break;} p=p.parentElement; }
            if(!inFixed) continue;
            var bt=(btn.innerText||btn.textContent||btn.getAttribute('aria-label')||'').trim();
            if(BROAD.test(bt) && fire(btn)) return true;
        }
        return false;
    }

    // Strategy 4: recurse into shadow roots.  Walk only direct children so
    // each element is visited exactly once — querySelectorAll('*') would visit
    // every descendant of every node, making this O(n²) on large DOMs.
    function collectRoots(el,out){
        if(el.shadowRoot) out.push(el.shadowRoot);
        var ch=el.shadowRoot?el.shadowRoot.children:el.children;
        for(var i=0;i<ch.length;i++) collectRoots(ch[i],out);
    }

    if(tryRoot(document)) return true;
    var roots=[];
    collectRoots(document.body,roots);
    for(var r=0;r<roots.length;r++){ if(tryRoot(roots[r])) return true; }
    return false;
})()
`

// jsWaitDOMStable is executed via ExecuteAsyncScript.  It installs a
// MutationObserver and resolves (calls the Selenium callback) once the DOM has
// had no mutations for stableMs milliseconds, or after maxMs at the latest.
// Both values are passed as arguments[0] and arguments[1].
const jsWaitDOMStable = `
var stableMs=arguments[0], maxMs=arguments[1], done=arguments[arguments.length-1];
var called=false;
function finish(){if(called)return;called=true;obs.disconnect();done();}
var timer=setTimeout(finish,stableMs);
var obs=new MutationObserver(function(){
    clearTimeout(timer);
    timer=setTimeout(finish,stableMs);
});
obs.observe(document.body,{childList:true,subtree:true,attributes:true,characterData:true});
setTimeout(function(){clearTimeout(timer);finish();},maxMs);
`

// waitAndClickSimplify polls for the Simplify extension's injected autofill
// button for up to timeout, clicking it as soon as it appears.  Returns true
// when the button was found and clicked.
//
// Two strategies run each poll tick in order:
//  1. JavaScript in the page context — handles any button injected directly
//     into the main DOM.
//  2. WebDriver SwitchToFrame walk — Simplify renders its popup inside a
//     moz-extension:// iframe that page-context JS cannot reach; WebDriver
//     crosses that boundary where JS cannot.
func waitAndClickSimplify(ctx context.Context, wd selenium.WebDriver, timeout time.Duration) bool {
	// Give the extension ~1 s to inject its overlay after the form is detected.
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Second):
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		res, err := wd.ExecuteScript(jsClickSimplify, nil)
		if err != nil {
			log.Printf("[simplify] jsClickSimplify error: %v", err)
		} else if clicked, ok := res.(bool); ok && clicked {
			log.Printf("[simplify] autofill button clicked (page context)")
			return true
		}
		if clickSimplifyInFrames(wd) {
			log.Printf("[simplify] autofill button clicked (extension iframe)")
			return true
		}
		// On the last tick, dump a DOM snapshot so we can see what Simplify
		// actually injected (custom elements, fixed overlays, shadow hosts).
		if time.Until(deadline) < time.Second {
			simplifyDOMDiag(wd)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// jsSimplifyDiag dumps any DOM elements that could be the Simplify overlay:
// custom elements (tag contains "-"), elements with "simplify" in any
// attribute, and all top-level fixed/absolute-positioned children of <body>.
const jsSimplifyDiag = `
(function(){
    var out=[];
    // 1. custom elements (non-standard tags contain a hyphen)
    document.querySelectorAll('*').forEach(function(el){
        if(el.tagName.indexOf('-')>-1){
            var attrs=[].map.call(el.attributes,function(a){return a.name+'='+a.value}).join(' ');
            out.push('custom-el: <'+el.tagName.toLowerCase()+'> '+attrs);
        }
    });
    // 2. elements with "simplify" in any attribute value
    document.querySelectorAll('*').forEach(function(el){
        [].forEach.call(el.attributes,function(a){
            if(/simplify/i.test(a.value)||/simplify/i.test(a.name)){
                out.push('simplify-attr: <'+el.tagName.toLowerCase()+'> '+a.name+'='+a.value);
            }
        });
    });
    // 3. fixed/absolute top-level children of body
    [].forEach.call(document.body.children,function(el){
        var pos=window.getComputedStyle(el).position;
        if(pos==='fixed'||pos==='absolute'||pos==='sticky'){
            var cls=el.className||'', id=el.id||'', tag=el.tagName.toLowerCase();
            out.push('overlay: <'+tag+'> id="'+id+'" class="'+cls+'"');
        }
    });
    return out.join('\n');
})()
`

func simplifyDOMDiag(wd selenium.WebDriver) {
	res, err := wd.ExecuteScript(jsSimplifyDiag, nil)
	if err != nil {
		log.Printf("[simplify-diag] script error: %v", err)
		return
	}
	dump, _ := res.(string)
	if dump == "" {
		log.Printf("[simplify-diag] no custom elements / overlays / simplify attributes found")
		return
	}
	for _, line := range strings.Split(dump, "\n") {
		log.Printf("[simplify-diag] %s", line)
	}
}

// clickSimplifyInFrames iterates every iframe on the page via WebDriver
// SwitchToFrame — which can access moz-extension:// frames that page JS
// cannot — and clicks the Simplify autofill button if found inside one.
// Always switches back to the default content before returning.
func clickSimplifyInFrames(wd selenium.WebDriver) bool {
	frames, err := wd.FindElements(selenium.ByCSSSelector, "iframe")
	if err != nil {
		log.Printf("[simplify] iframe lookup error: %v", err)
		return false
	}
	if len(frames) == 0 {
		return false
	}
	log.Printf("[simplify] scanning %d iframe(s) for autofill button", len(frames))
	for i, frame := range frames {
		clicked := func() bool {
			src, _ := frame.GetAttribute("src")
			if err := wd.SwitchFrame(frame); err != nil {
				log.Printf("[simplify] frame[%d] src=%q switch error: %v", i, src, err)
				return false
			}
			defer func() { _ = wd.SwitchFrame(nil) }()
			btns, _ := wd.FindElements(selenium.ByCSSSelector, `button,[role="button"]`)
			log.Printf("[simplify] frame[%d] src=%q buttons=%d", i, src, len(btns))
			for _, btn := range btns {
				text, _ := btn.Text()
				aria, _ := btn.GetAttribute("aria-label")
				label := strings.ToLower(strings.TrimSpace(text))
				if label == "" {
					label = strings.ToLower(strings.TrimSpace(aria))
				}
				if label != "" {
					log.Printf("[simplify] frame[%d] button label=%q", i, label)
				}
				if isSimplifyFillLabel(label) {
					if btn.Click() == nil {
						return true
					}
				}
			}
			return false
		}()
		if clicked {
			return true
		}
	}
	return false
}

// isSimplifyFillLabel reports whether a button label matches known Simplify
// autofill button text variants.
func isSimplifyFillLabel(s string) bool {
	for _, v := range []string{
		"autofill", "fill application", "fill form",
		"apply with simplify", "simplify autofill",
		"autofill application", "auto-fill",
	} {
		if s == v {
			return true
		}
	}
	return strings.Contains(s, "autofill") || strings.Contains(s, "simplify")
}

// waitForSimplifyDone blocks until the page DOM has been quiet for 600 ms
// (meaning Simplify has finished writing field values) or until maxWait elapses.
// It calls ExecuteScriptAsync directly (no goroutine) so there is never a
// leaked goroutine racing against subsequent WebDriver calls.  ctx is checked
// before the call; if already cancelled we return immediately.
func waitForSimplifyDone(ctx context.Context, wd selenium.WebDriver, maxWait time.Duration) {
	if ctx.Err() != nil {
		return
	}
	const stableMs = 600 // ms of DOM silence that counts as "done"
	maxMs := int(maxWait.Milliseconds())
	if maxMs <= 0 {
		maxMs = 10_000
	}
	// Set browser-side async timeout so ExecuteScriptAsync returns on its own
	// once maxMs elapses — no goroutine or select needed.
	_ = wd.SetAsyncScriptTimeout(maxWait + 2*time.Second)
	_, _ = wd.ExecuteScriptAsync(jsWaitDOMStable, []interface{}{stableMs, maxMs})
	log.Printf("[simplify] DOM stable — proceeding with supplemental fill")
}

func splitName(full string) (first, last string) {
	parts := strings.SplitN(strings.TrimSpace(full), " ", 2)
	first = parts[0]
	if len(parts) > 1 {
		last = parts[1]
	}
	return
}

func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
