package applier

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tebeka/selenium"
)


const challengeMaxWait = 30 * time.Second
const challengePollInterval = 500 * time.Millisecond

// ErrCaptcha is returned by FillApplication when an unsolvable CAPTCHA widget
// is detected inside the application form itself (as opposed to a full-page WAF
// wall that appears before the form loads).  The caller should treat this as a
// signal to apply a longer platform cooldown.
var ErrCaptcha = fmt.Errorf("captcha detected inside form")

// ErrEmailVerification is returned by FillApplication when the ATS (most
// commonly Greenhouse) has sent a verification code to the applicant's email
// and is waiting for it to be entered before the application can proceed.
// The caller should enforce a long cooldown (≥ 1 hour) so the platform does
// not repeatedly trigger new verification codes on every attempt.
var ErrEmailVerification = fmt.Errorf("email verification code required")

// challengeTitles are page-title substrings that indicate a WAF challenge page.
var challengeTitles = []string{
	"just a moment",            // Cloudflare
	"one moment please",        // Cloudflare
	"attention required",       // Cloudflare
	"checking your browser",    // Cloudflare / generic
	"verifying you are human",  // Various
	"ddos-guard",               // DDoS-Guard
	"please wait",              // Generic
}

// challengeBodySelectors are DOM elements whose presence signals a WAF
// challenge page.  Only include selectors that are exclusive to full-page
// bot-protection walls — NOT embedded CAPTCHAs that appear inside normal
// application forms (which would cause a false positive and block form filling).
var challengeBodySelectors = []string{
	"#cf-challenge-running",   // Cloudflare challenge active
	"#cf-please-wait",         // Cloudflare waiting room
	"#challenge-form",         // Cloudflare challenge form
	"#challenge-stage",        // Cloudflare Turnstile
	".cf-browser-verification", // Cloudflare browser check
}

// waitForChallenge checks whether the currently loaded page is a bot-protection
// challenge (Cloudflare Turnstile, hCaptcha, reCAPTCHA v2 checkbox, etc.) and
// tries to dismiss it automatically.
//
// Strategy:
//  1. Inject JS to hide navigator.webdriver so Cloudflare's fingerprint tests pass.
//  2. Every 500 ms, attempt to click any visible verification checkbox inside
//     known challenge iframes.
//  3. If auto-resolution succeeds within 30 s, return nil.
//  4. If headful is true and auto-resolution fails, print a prompt and wait
//     indefinitely for the human to solve it in the visible browser window.
//  5. If headful is false and auto-resolution fails, return an error.
//
// Call this immediately after every page navigation.
func waitForChallenge(wd selenium.WebDriver, headful bool) error {
	if !isChallengePage(wd) {
		return nil
	}
	log.Printf("[captcha] challenge page detected — attempting auto-resolution (max %s)", challengeMaxWait)

	// Mask the webdriver flag before anything else; Cloudflare reads it via JS.
	injectStealthJS(wd)

	deadline := time.Now().Add(challengeMaxWait)
	for time.Now().Before(deadline) {
		tryClickTurnstile(wd)
		tryClickHCaptcha(wd)
		tryClickRecaptchaV2(wd)

		time.Sleep(challengePollInterval)

		if !isChallengePage(wd) {
			log.Printf("[captcha] challenge cleared — continuing")
			return nil
		}
	}

	log.Printf("[captcha] auto-resolution failed — skipping URL")
	return fmt.Errorf("bot-protection challenge did not clear within %s", challengeMaxWait)
}

// isChallengePage returns true when the active page looks like a WAF / CAPTCHA wall.
func isChallengePage(wd selenium.WebDriver) bool {
	title, err := wd.Title()
	if err != nil {
		return false
	}
	low := strings.ToLower(title)
	for _, s := range challengeTitles {
		if strings.Contains(low, s) {
			return true
		}
	}
	for _, sel := range challengeBodySelectors {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			return true
		}
	}
	return false
}

// injectStealthJS overwrites navigator.webdriver with undefined so that
// Cloudflare's browser-fingerprint check does not see the automation flag.
func injectStealthJS(wd selenium.WebDriver) {
	_, _ = wd.ExecuteScript(`
		try {
			Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		} catch(e) {}
	`, nil)
}

// tryClickTurnstile switches into the Cloudflare Turnstile iframe and clicks
// the verification checkbox.  Turnstile's "managed" mode auto-verifies once
// the widget receives a real click event.
func tryClickTurnstile(wd selenium.WebDriver) {
	switchAndClick(wd,
		[]string{
			`iframe[src*="challenges.cloudflare.com"]`,
			`iframe[id*="cf-chl-widget"]`,
			`iframe[title*="Cloudflare security challenge"]`,
			`iframe[title*="Widget containing a Cloudflare"]`,
		},
		[]string{
			`input[type="checkbox"]`,
			`.ctp-checkbox-label`,
			`label[for*="turnstile"]`,
			`[id*="challenge"]`,
		},
	)
}

// tryClickHCaptcha switches into the hCaptcha challenge iframe and clicks
// the checkbox.  This handles the simple "I am human" tick; it does not
// solve image-selection challenges.
func tryClickHCaptcha(wd selenium.WebDriver) {
	switchAndClick(wd,
		[]string{
			`iframe[src*="hcaptcha.com"][src*="challenge"]`,
			`iframe[data-hcaptcha-widget-id]`,
		},
		[]string{
			`#checkbox`,
			`[aria-label*="hCaptcha"]`,
		},
	)
}

// tryClickRecaptchaV2 switches into the reCAPTCHA v2 anchor iframe and clicks
// the checkbox.  This only handles the simple checkbox variant; it does not
// solve image grid challenges.
func tryClickRecaptchaV2(wd selenium.WebDriver) {
	switchAndClick(wd,
		[]string{
			`iframe[src*="google.com/recaptcha"][src*="anchor"]`,
			`iframe[src*="recaptcha.net/recaptcha"][src*="anchor"]`,
		},
		[]string{
			`#recaptcha-anchor`,
			`.recaptcha-checkbox`,
		},
	)
}

// inFormCaptchaSelectors are DOM selectors that indicate a CAPTCHA widget
// rendered inside an application form (not a full-page WAF wall).
// Cloudflare Turnstile and hCaptcha are the two most common on Ashby.
var inFormCaptchaSelectors = []string{
	`iframe[src*="challenges.cloudflare.com"]`, // Cloudflare Turnstile widget
	`iframe[src*="hcaptcha.com"]`,              // hCaptcha widget
	`.cf-turnstile`,                            // Turnstile host element
	`.h-captcha`,                               // hCaptcha host element
	`[data-sitekey]`,                           // generic captcha container
}

// detectInFormCaptcha returns ErrCaptcha when a CAPTCHA widget is present
// inside the page that is not part of a full-page WAF wall.  It is intentionally
// distinct from isChallengePage, which targets full-page bot-protection screens.
func detectInFormCaptcha(wd selenium.WebDriver) error {
	for _, sel := range inFormCaptchaSelectors {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			log.Printf("[captcha] in-form captcha detected (%s) — aborting this URL", sel)
			return ErrCaptcha
		}
	}
	return nil
}

// switchAndClick finds the first iframe matching any selector in iframeSelectors,
// switches into it, clicks the first element matching any selector in
// innerSelectors, then switches back to the top-level frame.
func switchAndClick(wd selenium.WebDriver, iframeSelectors, innerSelectors []string) {
	var frame selenium.WebElement
	for _, sel := range iframeSelectors {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			frame = els[0]
			break
		}
	}
	if frame == nil {
		return
	}
	if err := wd.SwitchFrame(frame); err != nil {
		return
	}
	defer wd.SwitchFrame(nil) // always restore top-level frame

	for _, sel := range innerSelectors {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			_ = els[0].Click()
			return
		}
	}
}

// jsDetectEmailVerification inspects the current page for signs that the ATS
// has sent a verification code to the applicant's email and is waiting for it
// to be entered.  Greenhouse triggers this when it recognises the email address
// as belonging to an existing account or when it requires identity confirmation.
const jsDetectEmailVerification = `
(function(){
    var txt = ((document.body && document.body.innerText) || '').toLowerCase();
    var PHRASES = [
        'verification code','verify your email','check your email',
        'enter the code','enter your code','we sent a code',
        'sent to your email','email verification','confirm your email',
        'please verify','code sent to','sent you a','6-digit code',
        'enter the 6','one-time code','otp sent',
        // Two-factor / security-code variants (Greenhouse 2FA flow)
        'two-factor','two factor','2-factor','2fa',
        'two-step','two step','2-step',
        'security code','enter your security',
        'authentication code','authenticator',
    ];
    for (var i = 0; i < PHRASES.length; i++) {
        if (txt.indexOf(PHRASES[i]) !== -1) return true;
    }
    // Detect a short numeric/OTP-style input (4–8 chars, name/id/label hinting at "code").
    var inputs = document.querySelectorAll('input');
    for (var j = 0; j < inputs.length; j++) {
        var inp = inputs[j];
        if (inp.disabled || inp.type === 'hidden') continue;
        var nm = (inp.name || inp.id || inp.placeholder ||
                  inp.getAttribute('aria-label') || '').toLowerCase();
        if (/\b(verif|confirm.?code|token|otp|\bcode\b)/.test(nm)) return true;
        var ml = parseInt(inp.getAttribute('maxlength') || '0', 10);
        if (ml >= 4 && ml <= 8 && (inp.type === 'number' || inp.type === 'text' || inp.type === 'tel')) {
            // Only flag if the surrounding container also mentions "code" or "verify".
            var parent = inp.parentElement;
            var ctx = '';
            for (var k = 0; k < 3 && parent; k++) {
                ctx += (parent.textContent || '').toLowerCase();
                parent = parent.parentElement;
            }
            if (/verif|code|otp/.test(ctx)) return true;
        }
    }
    return false;
})()
`

// detectEmailVerification returns ErrEmailVerification when the current page
// shows signs that the ATS has sent a one-time verification code to the
// applicant's email and is waiting for input.
func detectEmailVerification(wd selenium.WebDriver) error {
	res, err := wd.ExecuteScript(jsDetectEmailVerification, nil)
	if err != nil {
		return nil // cannot determine — let the fill attempt continue
	}
	if detected, ok := res.(bool); ok && detected {
		log.Printf("[apply] email verification code prompt detected — aborting this URL")
		return ErrEmailVerification
	}
	return nil
}
