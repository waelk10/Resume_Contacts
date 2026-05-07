package applier

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"log"

	"github.com/tebeka/selenium"
	"github.com/tebeka/selenium/firefox"
)

// FillFlags controls form-fill behaviour for a single application.
type FillFlags struct {
	DryRun     bool // fill fields but do not click Submit
	Screenshot bool // save a PNG after filling
	Headful    bool // browser is visible — pause on dry-run so user can inspect form
	Hold       bool // keep window open until user closes it, then move to next URL
}

// Browser manages a single geckodriver process and spawns one Firefox
// WebDriver session per application.  Multiple goroutines may call
// FillApplication concurrently; each gets its own session.
type Browser struct {
	service *selenium.Service
	baseURL string
	caps    selenium.Capabilities
}

// NewBrowser locates geckodriver in PATH, starts it on port 4444, and
// configures Firefox capabilities.  Returns a clear error message with
// install instructions when geckodriver is not found.
func NewBrowser(headful bool) (*Browser, error) {
	geckodriverPath, err := exec.LookPath("geckodriver")
	if err != nil {
		return nil, fmt.Errorf(
			"geckodriver not found in PATH\n" +
				"Install it for your platform:\n" +
				"  Ubuntu/Debian : sudo apt install firefox-geckodriver\n" +
				"  Fedora/RHEL   : sudo dnf install geckodriver\n" +
				"  Arch Linux    : sudo pacman -S geckodriver\n" +
				"  macOS         : brew install geckodriver\n" +
				"  Manual        : https://github.com/mozilla/geckodriver/releases",
		)
	}

	const port = 4444
	svc, err := selenium.NewGeckoDriverService(geckodriverPath, port)
	if err != nil {
		return nil, fmt.Errorf("start geckodriver on :%d: %w\n(is port %d already in use?)", port, err, port)
	}

	ffCaps := firefox.Capabilities{
		// Hide the webdriver flag and automation extension from JavaScript so
		// Cloudflare's fingerprint checks are less likely to flag the session.
		Prefs: map[string]interface{}{
			"dom.webdriver.enabled":  false,
			"useAutomationExtension": false,
		},
	}
	if !headful {
		ffCaps.Args = []string{"-headless"}
	}
	caps := selenium.Capabilities{"browserName": "firefox"}
	caps.AddFirefox(ffCaps)

	// geckodriver speaks W3C WebDriver directly on /  — NOT the old /wd/hub path
	// used by Selenium standalone server.
	return &Browser{
		service: svc,
		baseURL: fmt.Sprintf("http://localhost:%d", port),
		caps:    caps,
	}, nil
}

// Close stops the geckodriver process.
func (b *Browser) Close() {
	_ = b.service.Stop()
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

	wd, err := selenium.NewRemote(b.caps, b.baseURL)
	if err != nil {
		return fmt.Errorf("open Firefox session: %w", err)
	}
	// In headful/hold mode keep the browser visible long enough to be useful
	// when an error occurs — otherwise defer wd.Quit() closes it instantly.
	defer func() {
		if retErr != nil {
			if flags.Hold {
				log.Printf("[hold] error — close the Firefox window to continue: %v", retErr)
				waitForWindowClose(ctx, wd)
				retErr = nil
			} else if flags.Headful {
				log.Printf("[apply] headful error — keeping browser open 10 s: %v", retErr)
				time.Sleep(10 * time.Second)
			}
		}
		_ = wd.Quit()
	}()

	_ = wd.SetPageLoadTimeout(30 * time.Second)
	// Zero implicit wait so FindElements returns immediately for absent fields
	// instead of blocking for several seconds on every miss.
	_ = wd.SetImplicitWaitTimeout(0)

	if err := wd.Get(job.URL); err != nil {
		return fmt.Errorf("navigate to %s: %w", job.URL, err)
	}
	// Give JavaScript-heavy ATS pages time to finish rendering.
	time.Sleep(2 * time.Second)

	// Dismiss any bot-protection challenge before trying to interact with the form.
	// In headful mode this blocks until the human solves it if auto-resolution fails.
	if err := waitForChallenge(wd, flags.Headful); err != nil {
		log.Printf("[captcha] %v — proceeding anyway", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	switch job.ATSPlatform {
	case "greenhouse":
		fillGreenhouse(ctx, wd, info, resumePath)
	case "lever":
		fillLever(ctx, wd, info, resumePath)
	case "ashby":
		fillAshby(ctx, wd, info, resumePath)
	case "bamboohr":
		fillBambooHR(ctx, wd, info, resumePath)
	default:
		fillGeneric(ctx, wd, info, resumePath)
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
	return clickSubmit(wd)
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

// ── ATS-specific form fillers ─────────────────────────────────────────────────

func fillGreenhouse(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	// Wait for the form to render before touching any field.
	if waitForElement(ctx, wd, "#first_name", 15*time.Second) == nil {
		return
	}
	first, last := splitName(info.Name)
	setInput(wd, "#first_name", first)
	setInput(wd, "#last_name", last)
	setInput(wd, "#email", info.Email)
	setInput(wd, "#phone", info.Phone)
	setInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL)
	uploadFile(wd, `input[name="resume"]`, resumePath)
}

func fillLever(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	if waitForElement(ctx, wd, `input[name="name"]`, 15*time.Second) == nil {
		return
	}
	first, last := splitName(info.Name)
	setInput(wd, `input[name="name"]`, info.Name)
	setInput(wd, `input[placeholder*="First" i]`, first)
	setInput(wd, `input[placeholder*="Last" i]`, last)
	setInput(wd, `input[name="email"]`, info.Email)
	setInput(wd, `input[name="phone"]`, info.Phone)
	setInput(wd, `input[name="urls[LinkedIn]"]`, info.LinkedInURL)
	// Lever exposes a single file input; try by name then fall back to type.
	if !tryUploadFile(wd, `input[name="resume"]`, resumePath) {
		tryUploadFile(wd, `input[type="file"]`, resumePath)
	}
}

func fillAshby(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	if waitForElement(ctx, wd, `input[name="name"]`, 15*time.Second) == nil {
		return
	}
	setInput(wd, `input[name="name"]`, info.Name)
	setInput(wd, `input[name="email"]`, info.Email)
	setInput(wd, `input[name="phone"]`, info.Phone)
	setInput(wd, `input[placeholder*="LinkedIn" i]`, info.LinkedInURL)
	uploadFile(wd, `input[type="file"]`, resumePath)
}

func fillBambooHR(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	if waitForElement(ctx, wd, `input[id*="firstName" i]`, 15*time.Second) == nil {
		return
	}
	first, last := splitName(info.Name)
	setInput(wd, `input[id*="firstName" i]`, first)
	setInput(wd, `input[id*="lastName" i]`, last)
	setInput(wd, `input[id*="email" i]`, info.Email)
	setInput(wd, `input[id*="phone" i]`, info.Phone)
	uploadFile(wd, `input[type="file"]`, resumePath)
}

func fillGeneric(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	// Wait for at least one text input to appear before trying to fill.
	if waitForElement(ctx, wd, `input[type="text"], input[type="email"], input[name]`, 15*time.Second) == nil {
		return
	}
	first, last := splitName(info.Name)

	// For each logical field, try selectors in priority order and stop at the first hit.
	for _, sel := range []string{
		`input[name*="first" i]`, `input[placeholder*="First name" i]`,
		`input[id*="first" i]`, `input[aria-label*="First" i]`,
	} {
		if trySetInput(wd, sel, first) {
			break
		}
	}
	for _, sel := range []string{
		`input[name*="last" i]`, `input[placeholder*="Last name" i]`,
		`input[id*="last" i]`, `input[aria-label*="Last" i]`,
	} {
		if trySetInput(wd, sel, last) {
			break
		}
	}
	for _, sel := range []string{
		`input[name="name"]`, `input[placeholder*="Full name" i]`,
	} {
		if trySetInput(wd, sel, info.Name) {
			break
		}
	}
	for _, sel := range []string{
		`input[type="email"]`, `input[name*="email" i]`,
		`input[placeholder*="email" i]`, `input[id*="email" i]`,
	} {
		if trySetInput(wd, sel, info.Email) {
			break
		}
	}
	for _, sel := range []string{
		`input[type="tel"]`, `input[name*="phone" i]`,
		`input[placeholder*="phone" i]`, `input[id*="phone" i]`,
	} {
		if trySetInput(wd, sel, info.Phone) {
			break
		}
	}
	for _, sel := range []string{
		`input[placeholder*="LinkedIn" i]`, `input[name*="linkedin" i]`,
	} {
		if trySetInput(wd, sel, info.LinkedInURL) {
			break
		}
	}
	uploadFile(wd, `input[type="file"]`, resumePath)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// jsFill scrolls the first element matching a CSS selector into view, then
// sets its value via the native HTMLInputElement/HTMLTextAreaElement prototype
// setter and fires input/change/blur so React, Vue, and Angular frameworks
// pick up the new value — plain SendKeys does not reliably trigger these.
const jsFill = `
var sel = arguments[0], val = arguments[1];
var el  = document.querySelector(sel);
if (!el) return;
el.scrollIntoView({block: 'center'});
try {
    var proto  = el.tagName === 'TEXTAREA'
        ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
    setter.call(el, val);
} catch (e) { el.value = val; }
el.dispatchEvent(new Event('input',  {bubbles: true}));
el.dispatchEvent(new Event('change', {bubbles: true}));
el.dispatchEvent(new Event('blur',   {bubbles: true}));
`

// jsSubmit finds and clicks the most likely submit button via JS heuristics,
// returning true when it clicked something.  Used as a last resort after all
// CSS/XPath attempts fail.
const jsSubmit = `
var WORDS = /\b(submit|apply|send|complete|finish|confirm)\b/i;
var all = Array.from(document.querySelectorAll(
    'button[type="submit"], input[type="submit"], button, [role="button"]'
));
for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (el.disabled || el.offsetParent === null) continue;
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

// setInput fills the first element matching selector; silently skips when absent.
func setInput(wd selenium.WebDriver, selector, value string) {
	trySetInput(wd, selector, value)
}

// trySetInput fills the first element matching selector, returning true when an
// element was found.  It uses a JS native-setter approach as the primary path
// (necessary for React/Vue controlled inputs) and falls back to WebDriver
// Click/Clear/SendKeys when ExecuteScript is unavailable.
func trySetInput(wd selenium.WebDriver, selector, value string) bool {
	if value == "" {
		return false
	}
	// Verify the element exists via WebDriver before going to JS so we can
	// return false immediately when no match — JS querySelector would also
	// return null, but checking here avoids a round-trip to the browser.
	els, err := wd.FindElements(selenium.ByCSSSelector, selector)
	if err != nil || len(els) == 0 {
		return false
	}
	if _, err = wd.ExecuteScript(jsFill, []interface{}{selector, value}); err != nil {
		// JS unavailable — fall back to click / clear / SendKeys.
		_ = els[0].Click()
		_ = els[0].Clear()
		_ = els[0].SendKeys(value)
	}
	return true
}

// uploadFile sets a file-input to an absolute path.
// geckodriver requires an absolute path to locate the file on disk.
func uploadFile(wd selenium.WebDriver, selector, path string) {
	tryUploadFile(wd, selector, path)
}

// tryUploadFile is like uploadFile but returns true when the element was found.
func tryUploadFile(wd selenium.WebDriver, selector, path string) bool {
	if path == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if _, err := os.Stat(absPath); err != nil {
		return false
	}
	els, err := wd.FindElements(selenium.ByCSSSelector, selector)
	if err != nil || len(els) == 0 {
		return false
	}
	_ = els[0].SendKeys(absPath)
	return true
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
			if cerr := els[0].Click(); cerr == nil {
				time.Sleep(2 * time.Second)
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
			if cerr := els[0].Click(); cerr == nil {
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}

	// 3. JS heuristic — last resort for non-standard markup.
	res, err := wd.ExecuteScript(jsSubmit, nil)
	if err == nil && res == true {
		time.Sleep(2 * time.Second)
		return nil
	}

	return fmt.Errorf("no submit button found on page")
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
