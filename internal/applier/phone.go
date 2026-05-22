package applier

import (
	"strings"

	"github.com/tebeka/selenium"
)

// phoneEntry maps a numeric dial code to whether the country's local dialling
// format omits the leading zero (noZero = true).  Most European, Middle
// Eastern, Australian, and South-East Asian countries use a leading 0 in their
// national format (noZero = false); US/Canada, Russia, and some others do not.
//
// Entries are sorted longest-code-first so that 3-digit codes like "972" are
// matched before the 2-digit "97" or 1-digit "9".
type phoneEntry struct {
	code   string
	noZero bool
}

var phoneCodes = []phoneEntry{
	// ── 3-digit codes ─────────────────────────────────────────────────────
	{"972", false}, // Israel
	{"971", false}, // UAE
	{"970", false}, // Palestine
	{"968", false}, // Oman
	{"967", false}, // Yemen
	{"966", false}, // Saudi Arabia
	{"965", false}, // Kuwait
	{"964", false}, // Iraq
	{"963", false}, // Syria
	{"962", false}, // Jordan
	{"961", false}, // Lebanon
	{"974", false}, // Qatar
	{"973", false}, // Bahrain
	{"880", false}, // Bangladesh
	{"856", false}, // Laos
	{"855", false}, // Cambodia
	{"853", false}, // Macao
	{"852", false}, // Hong Kong
	{"673", false}, // Brunei
	{"670", false}, // Timor-Leste
	{"598", false}, // Uruguay
	{"595", false}, // Paraguay
	{"593", false}, // Ecuador
	{"592", false}, // Guyana
	{"591", false}, // Bolivia
	{"509", false}, // Haiti
	{"507", false}, // Panama
	{"506", false}, // Costa Rica
	{"505", false}, // Nicaragua
	{"504", false}, // Honduras
	{"503", false}, // El Salvador
	{"502", false}, // Guatemala
	{"421", false}, // Slovakia
	{"420", false}, // Czech Republic
	{"389", false}, // North Macedonia
	{"387", false}, // Bosnia
	{"386", false}, // Slovenia
	{"385", false}, // Croatia
	{"382", false}, // Montenegro
	{"381", false}, // Serbia
	{"380", false}, // Ukraine
	{"375", false}, // Belarus
	{"374", false}, // Armenia
	{"373", false}, // Moldova
	{"372", false}, // Estonia
	{"371", false}, // Latvia
	{"370", false}, // Lithuania
	{"359", false}, // Bulgaria
	{"358", false}, // Finland
	{"357", false}, // Cyprus
	{"356", false}, // Malta
	{"355", false}, // Albania
	{"354", true},  // Iceland — 7-digit numbers, no leading 0
	{"353", false}, // Ireland
	{"352", false}, // Luxembourg
	{"351", false}, // Portugal
	// ── 2-digit codes ─────────────────────────────────────────────────────
	{"98", false}, // Iran
	{"95", false}, // Myanmar
	{"94", false}, // Sri Lanka
	{"93", false}, // Afghanistan
	{"92", false}, // Pakistan
	{"91", true},  // India — mobile numbers (7/8/9XX…) have no leading 0
	{"90", false}, // Turkey
	{"86", true},  // China — mobile numbers (1XX…) have no leading 0
	{"84", false}, // Vietnam
	{"82", false}, // South Korea
	{"81", false}, // Japan
	{"66", false}, // Thailand
	{"65", true},  // Singapore — 8-digit numbers, no leading 0
	{"64", false}, // New Zealand
	{"63", false}, // Philippines
	{"62", false}, // Indonesia
	{"61", false}, // Australia
	{"60", false}, // Malaysia
	{"57", true},  // Colombia — 10-digit numbers, no leading 0
	{"56", false}, // Chile
	{"55", true},  // Brazil — area code already embedded, no 0
	{"54", false}, // Argentina
	{"53", false}, // Cuba
	{"52", true},  // Mexico — 10-digit numbers, no leading 0
	{"51", true},  // Peru — no leading 0
	{"49", false}, // Germany
	{"48", false}, // Poland
	{"47", true},  // Norway — 8-digit numbers, no leading 0
	{"46", false}, // Sweden
	{"45", true},  // Denmark — 8-digit numbers, no leading 0
	{"44", false}, // UK
	{"43", false}, // Austria
	{"41", false}, // Switzerland
	{"40", false}, // Romania
	{"39", false}, // Italy
	{"36", false}, // Hungary
	{"34", true},  // Spain — 9-digit numbers, no leading 0
	{"33", false}, // France
	{"32", false}, // Belgium
	{"31", false}, // Netherlands
	{"30", true},  // Greece — 10-digit numbers, no leading 0
	{"27", false}, // South Africa
	{"20", false}, // Egypt
	// ── 1-digit codes ─────────────────────────────────────────────────────
	{"7", true}, // Russia / Kazakhstan — trunk prefix is 8, not 0
	{"1", true}, // US / Canada — no leading 0
}

// digitsOnly strips every non-digit character from s.
func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// splitPhone parses an international phone number (stored with or without a
// leading +, e.g. "972556661778" or "+44 20 7946 0958") into a dial code and
// national number.  For countries that use a leading 0 in their national
// dialling format, "0" is prepended to the subscriber part automatically.
// Returns ("", digits) when no known country code prefix is matched.
func splitPhone(phone string) (dialCode, localNumber string) {
	digits := digitsOnly(phone)
	if len(digits) == 0 {
		return "", phone
	}
	for _, e := range phoneCodes {
		if strings.HasPrefix(digits, e.code) {
			rest := digits[len(e.code):]
			if e.noZero {
				return e.code, rest
			}
			return e.code, "0" + rest
		}
	}
	return "", digits
}

// jsSetITINumber uses the intl-tel-input library (if present on the page) to
// set the international number on the first tel input it finds.  intl-tel-input
// automatically selects the correct country flag and national number.
// Returns true when an iti instance was found and set.
const jsSetITINumber = `
(function(intlNumber){
    var inputs = document.querySelectorAll('input[type="tel"], input[name*="phone" i]');
    for (var i = 0; i < inputs.length; i++) {
        var iti = window.intlTelInputGlobals && window.intlTelInputGlobals.getInstance(inputs[i]);
        if (!iti) continue;
        try { iti.setNumber(intlNumber); } catch(e) { continue; }
        return true;
    }
    return false;
})(arguments[0])
`

// jsSelectPhoneDialCode searches all <select> elements whose name or id
// contains common dial-code keywords and selects the option that matches the
// given numeric dial code.  Returns true when an option was selected.
const jsSelectPhoneDialCode = `
(function(code){
    var keywords = ['country_code','phone_country','dial_code','calling_code',
                    'phone_prefix','country-code','dialcode','countrycode',
                    'phonecode','phone_code','phoneext','phone_ext',
                    'countrycallingcode','callingcode'];
    var sels = document.querySelectorAll('select');
    for (var s = 0; s < sels.length; s++) {
        var el = sels[s];
        var nm = ((el.name || '') + ' ' + (el.id || '') + ' ' + (el.getAttribute('aria-label') || '')).toLowerCase();
        var match = false;
        for (var k = 0; k < keywords.length; k++) {
            if (nm.indexOf(keywords[k]) !== -1) { match = true; break; }
        }
        if (!match) {
            // Fallback: if the select has no label-matched name/id, check whether
            // its options look like dial codes (values or text contain "+NN" patterns).
            // Limit to selects that have at least 5 options to avoid false positives.
            if (el.options.length >= 5) {
                var dialLike = 0;
                for (var j = 0; j < Math.min(el.options.length, 20); j++) {
                    var ov = (el.options[j].value || '') + ' ' + (el.options[j].text || '');
                    if (/\+\d{1,3}/.test(ov)) dialLike++;
                }
                if (dialLike >= 3) match = true;
            }
        }
        if (!match) continue;
        var opts = el.options;
        for (var i = 0; i < opts.length; i++) {
            var v = (opts[i].value || '').replace(/\D/g,'');
            var t = opts[i].text || '';
            if (v === code || t.indexOf('+'+code) !== -1 ||
                t.indexOf('('+code+')') !== -1 || t.indexOf('(+'+code+')') !== -1) {
                el.value = opts[i].value;
                el.dispatchEvent(new Event('change',{bubbles:true}));
                return true;
            }
        }
    }
    return false;
})(arguments[0])
`

// trySelectDialCode attempts to set the country dial-code on the page by first
// trying intl-tel-input (which handles both flag and number), then a native
// <select>.  Returns (true, true) when iti handled everything (no separate
// phone-input fill needed), (false, true) when a native select was set (fill
// the phone input with the local number), and (false, false) when nothing was
// found (fill the phone input with the full number).
func trySelectDialCode(wd selenium.WebDriver, dialCode, intlNumber string) (itiHandled, selectFound bool) {
	res, err := wd.ExecuteScript(jsSetITINumber, []interface{}{"+" + intlNumber})
	if err == nil {
		if ok, _ := res.(bool); ok {
			return true, false
		}
	}
	res, err = wd.ExecuteScript(jsSelectPhoneDialCode, []interface{}{dialCode})
	if err == nil {
		if ok, _ := res.(bool); ok {
			return false, true
		}
	}
	return false, false
}

// fillPhoneInput writes number into the phone input using the JS native-value
// setter, falling back to label-based lookup.
func fillPhoneInput(wd selenium.WebDriver, number string) {
	for _, sel := range []string{
		`input[type="tel"]`,
		`[data-automation-id="phone-number"]`, // Workday
		`input[name*="phone" i]`,
		`input[id*="phone" i]`,
		`input[autocomplete="tel"]`,
		`input[placeholder*="phone" i]`,
	} {
		if trySetInput(wd, sel, number) {
			return
		}
	}
	tryFillByLabel(wd, "phone number", number)
	tryFillByLabel(wd, "phone", number)
	tryFillByLabel(wd, "mobile", number)
	tryFillByLabel(wd, "telefon", number)
	tryFillByLabel(wd, "handynummer", number)
}

// fillPhone fills the phone field on the current page, automatically detecting
// whether there is a separate country-code / dial-code selector:
//   - If intl-tel-input is present: setNumber handles everything.
//   - If a native <select> dial-code selector is present: select the code,
//     then fill the phone input with the national number (0 + subscriber).
//   - Otherwise: fill the phone input with the full international number.
func fillPhone(wd selenium.WebDriver, phone string) {
	if phone == "" {
		return
	}
	dialCode, localNumber := splitPhone(phone)
	digits := digitsOnly(phone)

	if dialCode != "" {
		itiHandled, selectFound := trySelectDialCode(wd, dialCode, digits)
		if itiHandled {
			return // intl-tel-input set both flag and number
		}
		if selectFound {
			fillPhoneInput(wd, localNumber)
			return
		}
	}
	fillPhoneInput(wd, digits)
}

// fillPhoneSendKeys is identical to fillPhone but dispatches real key events
// via WebDriver SendKeys on the tel input — necessary for ATS platforms (e.g.
// Ashby) whose custom phone components ignore the JS value setter.
func fillPhoneSendKeys(wd selenium.WebDriver, phone string) {
	if phone == "" {
		return
	}
	dialCode, localNumber := splitPhone(phone)
	digits := digitsOnly(phone)

	numberToType := digits
	if dialCode != "" {
		itiHandled, selectFound := trySelectDialCode(wd, dialCode, digits)
		if itiHandled {
			return
		}
		if selectFound {
			numberToType = localNumber
		}
	}

	// Prefer SendKeys on the first visible tel input.
	if els, _ := wd.FindElements(selenium.ByCSSSelector, `input[type="tel"]`); len(els) > 0 {
		if err := els[0].Click(); err == nil {
			_ = els[0].Clear()
			if els[0].SendKeys(numberToType) == nil {
				return
			}
		}
	}
	// Fall back to JS value setter.
	fillPhoneInput(wd, numberToType)
}
