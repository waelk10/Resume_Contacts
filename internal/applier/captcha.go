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

// challengeBodySelectors are DOM elements whose presence signals a challenge page.
var challengeBodySelectors = []string{
	"#cf-challenge-running",
	"#cf-please-wait",
	"#challenge-form",          // Cloudflare
	"#challenge-stage",         // Cloudflare Turnstile
	".cf-browser-verification",
	".h-captcha",               // hCaptcha embedded
	".g-recaptcha",             // reCAPTCHA embedded
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

	// Auto-resolution timed out.
	if headful {
		fmt.Println("\n[captcha] Auto-resolution failed. Please solve the CAPTCHA in the Firefox window, then press nothing — the program will continue automatically once the page clears.")
		for {
			time.Sleep(challengePollInterval)
			if !isChallengePage(wd) {
				log.Printf("[captcha] challenge cleared by human — continuing")
				return nil
			}
		}
	}

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
