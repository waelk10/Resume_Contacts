package applier

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// Browser manages a single geckodriver process and spawns one Firefox
// WebDriver session per application.  Multiple goroutines may call
// FillApplication concurrently; each gets its own independent session.
type Browser struct {
	service   *selenium.Service
	baseURL   string
	caps      selenium.Capabilities
	sessionMu sync.Mutex // serialises session creation; geckodriver queues internally but concurrent POSTs can race
}

// NewBrowser locates geckodriver in PATH, starts it on port 4444, and
// configures Firefox capabilities.  profileDir, when non-empty, is passed as
// the Firefox -profile argument so that extensions (e.g. Simplify) and their
// authentication cookies persist across sessions.  Returns a clear error
// message with install instructions when geckodriver is not found.
func NewBrowser(headful bool, profileDir string) (*Browser, error) {
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

	var args []string
	if !headful {
		args = append(args, "-headless")
	}
	if profileDir != "" {
		// Use the profile directory in-place so installed extensions and their
		// session cookies survive between geckodriver invocations.
		args = append(args, "-profile", profileDir)
	}
	// Do NOT set dom.webdriver.enabled or useAutomationExtension via Prefs —
	// Firefox 75+ locks dom.webdriver.enabled and geckodriver will refuse to
	// create the session with "Failed to set preferences".  The webdriver flag
	// is already masked at runtime by injectStealthJS (Object.defineProperty).
	ffCaps := firefox.Capabilities{Args: args}
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

// Close stops the geckodriver process.
func (b *Browser) Close() {
	_ = b.service.Stop()
}

// ErrWindowClosed is returned by FillApplication when the user closes the
// browser tab or window during a headful session.  processOne maps this to
// "skipped" rather than "error" so the URL is not added to the failure list.
var ErrWindowClosed = fmt.Errorf("window closed by user")

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

	b.sessionMu.Lock()
	wd, err := selenium.NewRemote(b.caps, b.baseURL)
	b.sessionMu.Unlock()
	if err != nil {
		return fmt.Errorf("open Firefox session: %w", err)
	}
	// In headful/hold mode keep the browser visible long enough to be useful
	// when an error occurs — otherwise defer wd.Quit() closes it instantly.
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

	// Dismiss any cookie consent banner before interacting with the page.
	// Cookie overlays block clicks on forms and Apply buttons; we accept them
	// unconditionally since we do not need to customise cookie preferences.
	dismissCookieBanner(wd)

	// Many job-description pages show the posting first and only reveal the
	// application form after the visitor clicks "Apply to this Job" or similar.
	// Detect this pattern and click through before attempting to fill.
	clickPreApplyIfNeeded(ctx, wd)

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
			// Simplify fills asynchronously — give it a moment to finish.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		} else {
			log.Printf("[simplify] autofill button not found — continuing with standard fill")
		}
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
	case "personio":
		fillPersonio(ctx, wd, info, resumePath)
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
	if err := clickSubmit(wd); err != nil {
		return err
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
// privacy, data processing, GDPR / DSGVO, or application consent.  Personio
// and several other ATS platforms make this checkbox mandatory; leaving it
// unticked prevents the Submit button from becoming active.
const jsAcceptPrivacyConsent = `
(function(){
    var CONSENT = /\b(privacy|data\s*(?:processing|protection|policy)|gdpr|dsgvo|datenschutz|personal\s+data|application\s+terms|consent\s+to\s+(?:the\s*)?(?:processing|collection))\b/i;
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

// fillCommonExtras fills the form fields that are common across all ATS
// platforms but are not covered by the ATS-specific fillers: address,
// professional links, cover letter, work authorization, EEO, and "how did
// you hear".  It is called after every ATS-specific filler.
func fillCommonExtras(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo) {
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
		for _, sel := range []string{
			`input[name*="country" i]`, `input[id*="country" i]`,
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
	if info.WorkAuthorized != "" || info.RequireSponsorship != "" {
		authAns := info.WorkAuthorized
		sponsorAns := info.RequireSponsorship
		if authAns == "" {
			authAns = "yes" // safe default: authorised; skip if not set
		}
		if sponsorAns == "" {
			sponsorAns = "no" // safe default: no sponsorship needed
		}
		wd.ExecuteScript(jsHandleWorkEligibility, []interface{}{authAns, sponsorAns}) //nolint:errcheck
	}

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
	if !trySetInput(wd, "#phone", info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	if !trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
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
	if !trySetInput(wd, `input[name="phone"]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	if !trySetInput(wd, `input[name="urls[LinkedIn]"]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
}

func fillAshby(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
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
	// Phone
	if !trySetInput(wd, `input[name="phone"]`, info.Phone) &&
		!trySetInput(wd, `input[type="tel"]`, info.Phone) &&
		!trySetInput(wd, `input[autocomplete="tel"]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	// LinkedIn
	if !trySetInput(wd, `input[placeholder*="LinkedIn" i]`, info.LinkedInURL) &&
		!trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
	fillCommonExtras(ctx, wd, info)
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
	if !trySetInput(wd, `input[id*="phone" i]`, info.Phone) &&
		!trySetInput(wd, `input[type="tel"]`, info.Phone) &&
		!trySetInput(wd, `input[name*="phone" i]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
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
	if !trySetInput(wd, `input[name="phone"]`, info.Phone) &&
		!trySetInput(wd, `input[type="tel"]`, info.Phone) &&
		!trySetInput(wd, `input[autocomplete="tel"]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
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

	filled = false
	for _, sel := range []string{
		`input[type="tel"]`, `input[name*="phone" i]`,
		`input[placeholder*="phone" i]`, `input[id*="phone" i]`,
	} {
		if trySetInput(wd, sel, info.Phone) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "phone", info.Phone) &&
			!tryFillByLabel(wd, "mobile", info.Phone) {
			if !tryFillByLabel(wd, "telefon", info.Phone) {
				tryFillByLabel(wd, "handynummer", info.Phone)
			}
		}
	}

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
		time.Sleep(400 * time.Millisecond) // wait for the overlay animation
	}
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
	`input[id*="first_name" i]`

// clickPreApplyIfNeeded detects the "job description first, form second"
// pattern: if the page has no form inputs yet it looks for an "Apply" trigger
// button, clicks it, then waits up to 10 s for the form to appear.
// Returns true when it performed a click (regardless of whether the form
// appeared — the caller's waitForElement inside the fill function will handle
// the remaining wait).
func clickPreApplyIfNeeded(ctx context.Context, wd selenium.WebDriver) bool {
	// Fast exit only when application-form inputs are already VISIBLE in the
	// viewport — not merely present in the DOM.  Many ATS platforms (BambooHR,
	// Greenhouse) pre-render the application form below the job description or
	// inside a hidden div; a plain FindElements call would match those hidden
	// inputs and cause us to skip the "Apply Now" button entirely.
	res, _ := wd.ExecuteScript(`
var els = document.querySelectorAll(arguments[0]);
for (var i = 0; i < els.length; i++) {
    var e = els[i];
    if (e.disabled) continue;
    if (e.offsetParent !== null || window.getComputedStyle(e).position === 'fixed') return true;
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
// It first reveals all hidden file inputs (a common ATS pattern where the real
// <input type="file"> is hidden behind a drag-drop zone), then walks through
// uploadSelectors from most specific to most generic, attempting SendKeys on
// every matching element until one succeeds.  Logs the outcome either way.
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

	// Reveal all hidden file inputs before FindElements so geckodriver can
	// reach them.  Ignore errors — reveal is best-effort.
	if n, err2 := wd.ExecuteScript(jsRevealFileInputs, nil); err2 == nil {
		if cnt, ok := n.(float64); ok && cnt > 0 {
			time.Sleep(150 * time.Millisecond) // let CSS transitions settle
		}
	}

	base := filepath.Base(absPath)
	for _, sel := range uploadSelectors {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		for _, el := range els {
			if err := el.SendKeys(absPath); err != nil {
				log.Printf("[upload] SendKeys failed on %q: %v", sel, err)
				continue
			}
			log.Printf("[upload] %q uploaded via selector %q", base, sel)
			return true
		}
	}

	log.Printf("[upload] WARNING: no file input found on page — resume not uploaded")
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
        ‘your application is complete’,’application is under review’
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
}

// errorURLSegs are path segments that indicate an error or failure page after
// a form submission redirect.
var errorURLSegs = []string{"/error", "/failed", "/failure", "/problem", "/oops"}

// verifySubmission waits up to 10 s after submit for a confirmation signal:
// a URL redirect to a thank-you page, a success phrase in the page text, or
// absence of validation errors.  On headful mode it keeps the window open an
// extra 15 s when a form error is detected so the user can see what went wrong.
// Returns nil when the submission appears successful or when no signal can be
// detected (some ATS platforms give no visible feedback).
func verifySubmission(ctx context.Context, wd selenium.WebDriver, headful bool) error {
	originalURL, _ := wd.CurrentURL()
	deadline := time.Now().Add(10 * time.Second)

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

// jsClickSimplify finds and clicks the Simplify extension's injected autofill
// button.  Simplify injects a floating panel into the page DOM (not a browser
// popup) so it is reachable via document.querySelector.  We try three
// strategies in order:
//  1. Elements whose class or id contains "simplify" and that are themselves
//     or contain a clickable button/role=button.
//  2. Any visible button whose full text matches known Simplify autofill labels.
//  3. Any button inside a fixed-position container (Simplify uses position:fixed
//     for its floating panel) whose text suggests an autofill action.
const jsClickSimplify = `
(function(){
    function vis(el){
        return el.offsetParent!==null || window.getComputedStyle(el).position==='fixed';
    }
    function tryClick(el){
        if(!el||el.disabled||!vis(el)) return false;
        el.scrollIntoView({block:'center'});
        el.click();
        return true;
    }

    // Strategy 1: elements with "simplify" in class/id that are or contain buttons
    var simplifyEls = document.querySelectorAll(
        'button[class*="simplify" i], button[id*="simplify" i],' +
        '[class*="simplify" i] button, [id*="simplify" i] button,' +
        '[class*="simplify" i][role="button"], [id*="simplify" i][role="button"],' +
        '[data-simplify] button, [data-extension="simplify"] button'
    );
    for(var i=0;i<simplifyEls.length;i++){
        if(tryClick(simplifyEls[i])) return true;
    }

    // Strategy 2: text-based search — Simplify's button text variants
    var FILL=/^\s*(autofill|fill\s+application|fill\s+form|apply\s+with\s+simplify|simplify\s+autofill|autofill\s+application)\s*$/i;
    var btns=document.querySelectorAll('button,[role="button"]');
    for(var j=0;j<btns.length;j++){
        var t=(btns[j].innerText||btns[j].textContent||btns[j].getAttribute('aria-label')||'').trim();
        if(FILL.test(t) && tryClick(btns[j])) return true;
    }

    // Strategy 3: any button in a fixed-position ancestor whose text includes
    // "autofill" or "simplify" — covers future Simplify UI variants
    var BROAD=/simplify|autofill/i;
    for(var k=0;k<btns.length;k++){
        var btn=btns[k];
        if(!vis(btn)) continue;
        var p=btn.parentElement;
        var inFixed=false;
        while(p){
            if(window.getComputedStyle(p).position==='fixed'){inFixed=true;break;}
            p=p.parentElement;
        }
        if(!inFixed) continue;
        var bt=(btn.innerText||btn.textContent||btn.getAttribute('aria-label')||'').trim();
        if(BROAD.test(bt) && tryClick(btn)) return true;
    }
    return false;
})()
`

// waitAndClickSimplify polls for the Simplify extension's injected autofill
// button for up to timeout, clicking it as soon as it appears.  Returns true
// when the button was found and clicked.
func waitAndClickSimplify(ctx context.Context, wd selenium.WebDriver, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		res, err := wd.ExecuteScript(jsClickSimplify, nil)
		if err == nil && res == true {
			log.Printf("[simplify] autofill button clicked")
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
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
