package output

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"Resume_Contacts_Scraper/internal/contact"
)

// ReadAllContacts parses every *.vcf file in dir and returns all contacts in
// file order.
func ReadAllContacts(dir string) ([]contact.Contact, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".vcf") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)

	var contacts []contact.Contact
	for _, p := range paths {
		cs, parseErr := parseVCF(p)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", p, parseErr)
		}
		contacts = append(contacts, cs...)
	}
	return contacts, nil
}

// FilterByEmailRegex removes every contact whose email matches pattern.
func FilterByEmailRegex(contacts []contact.Contact, pattern *regexp.Regexp) []contact.Contact {
	out := make([]contact.Contact, 0, len(contacts))
	for _, c := range contacts {
		if !pattern.MatchString(c.Email) {
			out = append(out, c)
		}
	}
	return out
}

// DeduplicateByEmail retains, for each unique email (case-insensitive), the
// contact with the most populated fields (Name, Org, Source), using total
// character length as a tiebreaker. Insertion order among winners is preserved.
func DeduplicateByEmail(contacts []contact.Contact) []contact.Contact {
	type entry struct {
		c     contact.Contact
		score int
	}
	best := make(map[string]entry, len(contacts))
	order := make([]string, 0, len(contacts))

	for _, c := range contacts {
		key := strings.ToLower(c.Email)
		s := infoScore(c)
		if prev, ok := best[key]; !ok {
			best[key] = entry{c: c, score: s}
			order = append(order, key)
		} else if s > prev.score {
			best[key] = entry{c: c, score: s}
		}
	}

	out := make([]contact.Contact, 0, len(order))
	for _, k := range order {
		out = append(out, best[k].c)
	}
	return out
}

// RewriteContacts deletes all *.vcf files in dir and rewrites them from
// contacts in chunked order.
func RewriteContacts(dir string, contacts []contact.Contact) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".vcf") {
			if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr != nil {
				return rmErr
			}
		}
	}

	if len(contacts) == 0 {
		return nil
	}

	chunkIdx := 1
	chunkCount := 0
	var f *os.File

	for _, c := range contacts {
		if f == nil || chunkCount >= chunkSize {
			if f != nil {
				if closeErr := f.Close(); closeErr != nil {
					return closeErr
				}
				chunkIdx++
			}
			f, err = os.OpenFile(chunkPath(dir, chunkIdx), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				return err
			}
			chunkCount = 0
		}

		_, err = fmt.Fprintf(f,
			"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:%s\r\nEMAIL:%s\r\nORG:%s\r\nSOURCE:%s\r\nEND:VCARD\r\n",
			vcfEscape(displayName(c)), c.Email, vcfEscape(c.Org), c.Source,
		)
		if err != nil {
			f.Close()
			return err
		}
		chunkCount++
	}

	if f != nil {
		return f.Close()
	}
	return nil
}

// infoScore scores how much useful information a contact carries.
// Each non-empty field contributes 1000 plus its length so that a longer
// value beats a missing one, but any value beats absence.
func infoScore(c contact.Contact) int {
	score := 0
	if c.Name != "" {
		score += 1000 + len(c.Name)
	}
	if c.Org != "" {
		score += 1000 + len(c.Org)
	}
	if c.Source != "" {
		score += 1000 + len(c.Source)
	}
	return score
}

// parseVCF reads a VCF file and returns all vCard entries it contains.
func parseVCF(path string) ([]contact.Contact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var contacts []contact.Contact
	var cur *contact.Contact
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		upper := strings.ToUpper(line)
		switch {
		case upper == "BEGIN:VCARD":
			cur = &contact.Contact{}
		case upper == "END:VCARD":
			if cur != nil {
				contacts = append(contacts, *cur)
				cur = nil
			}
		case cur != nil && strings.HasPrefix(upper, "FN:"):
			cur.Name = vcfUnescape(line[3:])
		case cur != nil && strings.HasPrefix(upper, "EMAIL:"):
			cur.Email = line[6:]
		case cur != nil && strings.HasPrefix(upper, "ORG:"):
			cur.Org = vcfUnescape(line[4:])
		case cur != nil && strings.HasPrefix(upper, "SOURCE:"):
			cur.Source = line[7:]
		}
	}
	return contacts, scanner.Err()
}

// vcfUnescape reverses the escaping applied by vcfEscape.
func vcfUnescape(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\;`, ";")
	s = strings.ReplaceAll(s, `\,`, ",")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}
