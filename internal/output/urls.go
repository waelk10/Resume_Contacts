package output

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// URLWriter appends deduplicated URLs to a plain-text file, one per line.
// Re-opening an existing file resumes without duplicates.
type URLWriter struct {
	mu    sync.Mutex
	path  string
	file  *os.File
	seen  map[string]struct{}
	count int
}

func NewURLWriter(path string) (*URLWriter, error) {
	seen, err := loadSeenURLs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &URLWriter{path: path, file: f, seen: seen}, nil
}

// Write appends rawURL on its own line if it has not been seen before.
// Returns true when written, false when a duplicate is skipped.
func (w *URLWriter) Write(rawURL string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.resetIfDeleted(); err != nil {
		return false, err
	}
	key := strings.ToLower(rawURL)
	if _, dup := w.seen[key]; dup {
		return false, nil
	}
	w.seen[key] = struct{}{}
	if _, err := fmt.Fprintln(w.file, rawURL); err != nil {
		return false, err
	}
	w.count++
	return true, nil
}

// resetIfDeleted checks whether the output file still exists at w.path.
// If it has been deleted, the seen set and count are cleared and a new file
// is created, so the next write starts with a blank slate.
// Must be called with w.mu held.
func (w *URLWriter) resetIfDeleted() error {
	if _, err := os.Stat(w.path); !os.IsNotExist(err) {
		return nil
	}
	_ = w.file.Close()
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.seen = make(map[string]struct{})
	w.count = 0
	log.Printf("[url-writer] output file deleted; restarting fresh at %s", w.path)
	return nil
}

func (w *URLWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

func (w *URLWriter) Close() error {
	return w.file.Close()
}

func loadSeenURLs(path string) (map[string]struct{}, error) {
	seen := make(map[string]struct{})
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return seen, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			seen[strings.ToLower(line)] = struct{}{}
		}
	}
	return seen, scanner.Err()
}
