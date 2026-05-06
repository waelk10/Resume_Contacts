package scraper

// taggedSeed is a seed URL annotated with the country/region codes it covers.
// Codes are ISO 3166-1 alpha-2 (lowercase) plus two special values:
//   - "global"  boards with no geographic focus
//   - "eu"      pan-European boards not tied to a single country
//
// Pass these codes (or region aliases) via Config.Countries to restrict which
// seeds are used.  An empty Countries slice means "use all seeds" (default).
type taggedSeed struct {
	URL       string
	Countries []string
}

// regionAliases expand shorthand region names into the country codes they cover.
// A seed is included when its country tags intersect the expanded filter set.
var regionAliases = map[string][]string{
	"dach":     {"de", "at", "ch"},
	"benelux":  {"nl", "be", "lu"},
	"nordics":  {"dk", "se", "no", "fi", "is"},
	"cee":      {"pl", "cz", "hu", "ro", "bg", "hr", "si", "sk"},
	"southern": {"es", "pt", "it", "gr", "mt"},
	// "eu" includes every EU-adjacent country code plus the "eu" tag itself so
	// pan-European seeds (tagged "eu") are always pulled in when eu is requested.
	"eu": {
		"eu",
		"de", "at", "ch",
		"nl", "be", "lu",
		"fr",
		"es", "pt", "it", "gr", "mt",
		"dk", "se", "no", "fi", "is",
		"pl", "cz", "hu", "ro", "bg", "hr", "si", "sk",
		"gb", "ie",
	},
}

// expandCountries converts a slice of user-supplied codes (which may include
// region aliases) into a flat set of canonical codes used for seed filtering.
// Returns a nil map when codes is empty, which the caller treats as "no filter".
func expandCountries(codes []string) map[string]bool {
	if len(codes) == 0 {
		return nil
	}
	out := make(map[string]bool, len(codes)*4)
	for _, code := range codes {
		if expanded, ok := regionAliases[code]; ok {
			for _, c := range expanded {
				out[c] = true
			}
		} else {
			out[code] = true
		}
	}
	return out
}

// seedMatchesFilter reports whether a seed's country tags intersect the filter.
func seedMatchesFilter(countries []string, filter map[string]bool) bool {
	for _, c := range countries {
		if filter[c] {
			return true
		}
	}
	return false
}

// webSeeds is the built-in list of seed URLs annotated with country/region tags.
var webSeeds = []taggedSeed{
	// ── Global ──────────────────────────────────────────────────────────────────
	{URL: "https://remoteok.com", Countries: []string{"global"}},
	{URL: "https://weworkremotely.com/listings", Countries: []string{"global"}},
	{URL: "https://startup.jobs", Countries: []string{"global"}},
	{URL: "https://wellfound.com/jobs", Countries: []string{"global"}},
	{URL: "https://www.workatastartup.com/jobs", Countries: []string{"global"}},
	{URL: "https://remotive.com/remote-jobs", Countries: []string{"global"}},

	// ── EU / Pan-European ────────────────────────────────────────────────────────
	{URL: "https://eu-startups.com/jobs", Countries: []string{"eu"}},
	{URL: "https://otta.com/jobs", Countries: []string{"eu", "gb"}},
	{URL: "https://www.honeypot.io/pages/jobs", Countries: []string{"eu", "de", "nl", "at"}},
	{URL: "https://landing.jobs/jobs", Countries: []string{"eu", "pt"}},
	{URL: "https://relocate.me/jobs", Countries: []string{"eu"}},
	{URL: "https://www.jobgether.com/en/jobs", Countries: []string{"eu"}},
	{URL: "https://nofluffjobs.com/jobs", Countries: []string{"eu", "pl", "cz", "sk", "ro"}},
	{URL: "https://eurojobs.com/jobs", Countries: []string{"eu"}},
	{URL: "https://tech.eu/jobs", Countries: []string{"eu"}},
	{URL: "https://berlinstartupjobs.com", Countries: []string{"de"}},
	{URL: "https://amsterdamtechjobs.com", Countries: []string{"nl"}},
	{URL: "https://jobs.techcorridor.eu", Countries: []string{"eu", "pl", "cz"}},

	// ── UK ──────────────────────────────────────────────────────────────────────
	{URL: "https://www.totaljobs.com/jobs/it-jobs", Countries: []string{"gb"}},
	{URL: "https://www.reed.co.uk/jobs/it-jobs", Countries: []string{"gb"}},
	{URL: "https://www.cwjobs.co.uk/jobs", Countries: []string{"gb"}},
	{URL: "https://www.itjobswatch.co.uk", Countries: []string{"gb"}},
	{URL: "https://www.jobserve.com/gb/en/IT-Jobs", Countries: []string{"gb"}},
	{URL: "https://www.glassdoor.co.uk/Job/uk-software-engineer-jobs", Countries: []string{"gb"}},

	// ── Ireland ─────────────────────────────────────────────────────────────────
	{URL: "https://www.irishjobs.ie/tech", Countries: []string{"ie"}},
	{URL: "https://www.engineerjobs.ie/it-jobs", Countries: []string{"ie"}},

	// ── France ──────────────────────────────────────────────────────────────────
	{URL: "https://www.welcometothejungle.com/en/jobs", Countries: []string{"fr", "eu"}},
	{URL: "https://www.apec.fr/candidat/recherche-offre.html?domaine=Informatique", Countries: []string{"fr"}},
	{URL: "https://www.remixjobs.com", Countries: []string{"fr"}},
	{URL: "https://www.francejobs.com/en/jobs/technology", Countries: []string{"fr"}},
	{URL: "https://www.glassdoor.fr/Emploi/france-developpeur-logiciel", Countries: []string{"fr"}},
	{URL: "https://www.welcometothejungle.com/companies", Countries: []string{"fr", "eu"}},
	{URL: "https://www.ouishare.net/jobs", Countries: []string{"eu", "fr"}},

	// ── DACH: Germany ───────────────────────────────────────────────────────────
	{URL: "https://www.stepstone.de/jobs/en", Countries: []string{"de"}},
	{URL: "https://www.jobs.de", Countries: []string{"de"}},
	{URL: "https://www.monster.de/jobs/q-it", Countries: []string{"de"}},
	{URL: "https://www.indeed.de/Jobs?q=software", Countries: []string{"de"}},
	{URL: "https://www.workflowjobs.com", Countries: []string{"de"}},
	{URL: "https://www.berlinstartupjobs.com/companies", Countries: []string{"de"}},
	{URL: "https://jobs.munichstartup.com", Countries: []string{"de"}},
	{URL: "https://www.hamburg-startups.de/jobs", Countries: []string{"de"}},
	{URL: "https://www.glassdoor.de/Job/deutschland-software-entwickler-jobs", Countries: []string{"de"}},

	// ── DACH: Austria ───────────────────────────────────────────────────────────
	{URL: "https://www.karriere.at/jobs", Countries: []string{"at"}},

	// ── DACH: Switzerland ───────────────────────────────────────────────────────
	{URL: "https://www.swissdevjobs.ch/jobs", Countries: []string{"ch"}},
	{URL: "https://www.jobs.ch/en/tech", Countries: []string{"ch"}},

	// ── Benelux: Netherlands ────────────────────────────────────────────────────
	{URL: "https://www.nationalevacaturebank.nl/it-banen", Countries: []string{"nl"}},
	{URL: "https://www.techpays.eu/jobs", Countries: []string{"eu", "nl", "be", "lu"}},

	// ── Benelux: Belgium ────────────────────────────────────────────────────────
	{URL: "https://www.jobat.be/en/jobs", Countries: []string{"be"}},
	{URL: "https://www.vdab.be/jobs", Countries: []string{"be"}},
	{URL: "https://www.ictjob.be/en", Countries: []string{"be"}},

	// ── Benelux: Luxembourg ─────────────────────────────────────────────────────
	{URL: "https://jobs.lu/en", Countries: []string{"lu"}},
	{URL: "https://www.startupjobs.lu", Countries: []string{"lu"}},

	// ── Nordics: Denmark ────────────────────────────────────────────────────────
	{URL: "https://www.jobindex.dk/jobsoegning", Countries: []string{"dk"}},
	{URL: "https://www.thehub.io/jobs", Countries: []string{"dk", "se", "no"}},
	{URL: "https://www.jobylon.com/jobs", Countries: []string{"eu", "se", "no", "dk"}},

	// ── Nordics: Sweden ─────────────────────────────────────────────────────────
	{URL: "https://www.thelocal.se/jobs", Countries: []string{"se"}},
	{URL: "https://www.academicwork.se/jobs/tech", Countries: []string{"se"}},

	// ── Nordics: Norway ─────────────────────────────────────────────────────────
	{URL: "https://www.finn.no/job/fulltime/search.html", Countries: []string{"no"}},
	{URL: "https://www.engineer.no/jobb", Countries: []string{"no"}},

	// ── Nordics: Finland ────────────────────────────────────────────────────────
	{URL: "https://www.duunitori.fi/tyopaikat/ohjelmointi", Countries: []string{"fi"}},
	{URL: "https://www.ictuutiset.fi/tyopaikat", Countries: []string{"fi"}},

	// ── Nordics: Iceland ────────────────────────────────────────────────────────
	{URL: "https://www.vinnur.is/jobs/technology", Countries: []string{"is"}},

	// ── Southern Europe: Spain ──────────────────────────────────────────────────
	{URL: "https://www.tecnoempleo.com", Countries: []string{"es"}},
	{URL: "https://www.jobsinbarcelona.es", Countries: []string{"es"}},
	{URL: "https://www.linkedin.com/jobs/search/?geoId=101282230", Countries: []string{"es"}},

	// ── Southern Europe: Portugal ───────────────────────────────────────────────
	{URL: "https://www.itjobs.pt", Countries: []string{"pt"}},
	{URL: "https://www.landing.jobs/jobs", Countries: []string{"pt", "eu"}},

	// ── Southern Europe: Italy ──────────────────────────────────────────────────
	{URL: "https://www.infojobs.it/offerta-lavoro/informatica", Countries: []string{"it"}},
	{URL: "https://www.trovolavoro.it/annunci/informatica", Countries: []string{"it"}},
	{URL: "https://www.linkedin.com/jobs/search/?geoId=101620260", Countries: []string{"it"}},

	// ── Southern Europe: Greece ─────────────────────────────────────────────────
	{URL: "https://www.kariera.gr/jobs/technology", Countries: []string{"gr"}},

	// ── Southern Europe: Malta ──────────────────────────────────────────────────
	{URL: "https://www.jobsinmalta.com/sectors/it", Countries: []string{"mt"}},

	// ── CEE: Poland ─────────────────────────────────────────────────────────────
	{URL: "https://justjoin.it", Countries: []string{"pl"}},
	{URL: "https://pracuj.it", Countries: []string{"pl"}},

	// ── CEE: Czech Republic ─────────────────────────────────────────────────────
	{URL: "https://www.startupjobs.cz", Countries: []string{"cz"}},
	{URL: "https://www.jobs.cz/it", Countries: []string{"cz"}},

	// ── CEE: Hungary ────────────────────────────────────────────────────────────
	{URL: "https://www.profession.hu/allasok/informatika", Countries: []string{"hu"}},

	// ── CEE: Romania ────────────────────────────────────────────────────────────
	{URL: "https://www.bestjobs.eu/ro/locuri-de-munca/it", Countries: []string{"ro"}},

	// ── CEE: Bulgaria ───────────────────────────────────────────────────────────
	{URL: "https://www.dev.bg/jobs", Countries: []string{"bg"}},

	// ── CEE: Croatia ────────────────────────────────────────────────────────────
	{URL: "https://www.moj-posao.net/Poslovi/IT/", Countries: []string{"hr"}},

	// ── CEE: Slovenia ───────────────────────────────────────────────────────────
	{URL: "https://www.sloveniajobs.si/jobs/technology", Countries: []string{"si"}},

	// ── CEE: Slovakia ───────────────────────────────────────────────────────────
	{URL: "https://www.profesia.sk/praca/informacne-technologie", Countries: []string{"sk"}},

	// ── Global aggregators ──────────────────────────────────────────────────────
	{URL: "https://www.linkedin.com/jobs/collections/recommended/", Countries: []string{"global"}},
	{URL: "https://www.producthunt.com/jobs", Countries: []string{"global"}},
}
