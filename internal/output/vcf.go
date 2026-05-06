package output

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"Resume_Contacts_Scraper/internal/contact"
)

const chunkSize = 100

// VCFWriter writes vCard entries into a directory, splitting output into files
// of chunkSize entries each (contacts_001.vcf, contacts_002.vcf, …).
// Duplicate emails are skipped across all existing files on resume.
type VCFWriter struct {
	mu         sync.Mutex
	dir        string
	file       *os.File
	seen       map[string]struct{}
	total      int
	chunkIdx   int
	chunkCount int
}

func NewVCFWriter(dir string) (*VCFWriter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	seen, lastIdx, lastCount, err := scanDir(dir)
	if err != nil {
		return nil, err
	}
	// No files yet, or the last file is already full — open a fresh chunk.
	if lastIdx == 0 || lastCount >= chunkSize {
		lastIdx++
		lastCount = 0
	}
	f, err := openChunk(dir, lastIdx, lastCount > 0)
	if err != nil {
		return nil, err
	}
	return &VCFWriter{
		dir:        dir,
		file:       f,
		seen:       seen,
		chunkIdx:   lastIdx,
		chunkCount: lastCount,
	}, nil
}

// Write appends the contact as a vCard entry, rotating to a new file every
// chunkSize entries. Returns true when written, false when a duplicate is skipped.
func (w *VCFWriter) Write(c contact.Contact) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := strings.ToLower(c.Email)
	if _, dup := w.seen[key]; dup {
		return false, nil
	}
	w.seen[key] = struct{}{}

	if w.chunkCount >= chunkSize {
		if err := w.file.Close(); err != nil {
			return false, err
		}
		w.chunkIdx++
		w.chunkCount = 0
		f, err := openChunk(w.dir, w.chunkIdx, false)
		if err != nil {
			return false, err
		}
		w.file = f
	}

	_, err := fmt.Fprintf(w.file,
		"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:%s\r\nEMAIL:%s\r\nORG:%s\r\nSOURCE:%s\r\nEND:VCARD\r\n",
		vcfEscape(displayName(c)), c.Email, vcfEscape(c.Org), c.Source,
	)
	if err != nil {
		return false, err
	}
	w.chunkCount++
	w.total++
	return true, nil
}

func (w *VCFWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

func (w *VCFWriter) Close() error {
	return w.file.Close()
}

// chunkPath returns the full path for chunk index idx (1-based).
func chunkPath(dir string, idx int) string {
	return filepath.Join(dir, fmt.Sprintf("contacts_%03d.vcf", idx))
}

func openChunk(dir string, idx int, appendMode bool) (*os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(chunkPath(dir, idx), flags, 0644)
}

// scanDir reads all *.vcf files in dir, populates seen with known emails, and
// returns the last chunk's index and entry count so the writer can resume.
func scanDir(dir string) (seen map[string]struct{}, lastIdx, lastCount int, err error) {
	seen = make(map[string]struct{})
	entries, readErr := os.ReadDir(dir)
	if os.IsNotExist(readErr) || len(entries) == 0 {
		return seen, 0, 0, nil
	}
	if readErr != nil {
		return nil, 0, 0, readErr
	}

	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".vcf") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		count, scanErr := scanVCF(p, seen)
		if scanErr != nil {
			return nil, 0, 0, scanErr
		}
		// Extract the numeric suffix from contacts_NNN.vcf.
		base := strings.TrimSuffix(filepath.Base(p), ".vcf")
		parts := strings.Split(base, "_")
		if idx, convErr := strconv.Atoi(parts[len(parts)-1]); convErr == nil && idx > lastIdx {
			lastIdx = idx
			lastCount = count
		}
	}
	return seen, lastIdx, lastCount, nil
}

// scanVCF records all EMAIL: values from path into seen and returns the entry count.
func scanVCF(path string, seen map[string]struct{}) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		upper := strings.ToUpper(line)
		switch {
		case upper == "BEGIN:VCARD":
			count++
		case strings.HasPrefix(upper, "EMAIL:"):
			seen[strings.ToLower(line[6:])] = struct{}{}
		}
	}
	return count, scanner.Err()
}

// displayName returns the best available human-readable name for a contact.
// Priority: scraped name → local-part parse → org (always non-empty).
func displayName(c contact.Contact) string {
	if c.Name != "" {
		return c.Name
	}
	if n := nameFromLocalPart(c.Email); n != "" {
		return n
	}
	return c.Org
}

// nameFromLocalPart tries to derive a human name from the email's local-part.
// It only produces output when the local-part contains a separator (. _ -)
// so that role addresses like "hr" or "recruiting" fall through to the Org
// fallback rather than being mis-used as names.
func nameFromLocalPart(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 || !strings.ContainsAny(email[:at], "._-") {
		return ""
	}
	parts := strings.FieldsFunc(email[:at], func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) < 2 || !isAlphaOnly(p) {
			return "" // single initial or digit-mixed segment — not a name
		}
		out = append(out, strings.ToUpper(p[:1])+strings.ToLower(p[1:]))
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " ")
}

// isAlphaOnly returns true when every byte in s is an ASCII letter.
func isAlphaOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func vcfEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ",", `\,`)
	s = strings.ReplaceAll(s, ";", `\;`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
