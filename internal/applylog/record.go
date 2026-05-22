package applylog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Record is one application outcome, serialised as a JSONL line.
type Record struct {
	RunID    string    `json:"run_id"`
	TS       time.Time `json:"ts"`
	Status   string    `json:"status"` // applied|dry-run|skipped|error
	Title    string    `json:"title,omitempty"`
	Company  string    `json:"company,omitempty"`
	URL      string    `json:"url,omitempty"`
	Platform string    `json:"platform,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Writer appends Records to a JSONL file.
type Writer struct {
	f *os.File
	w *bufio.Writer
}

// NewWriter opens path for appending and returns a Writer.
func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, w: bufio.NewWriter(f)}, nil
}

// NewWriterToFile wraps an already-open file (for atomic writes).
func NewWriterToFile(f *os.File) *Writer {
	return &Writer{f: f, w: bufio.NewWriter(f)}
}

func (w *Writer) Write(r Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w.w, "%s\n", data); err != nil {
		return err
	}
	return w.w.Flush()
}

func (w *Writer) Close() error {
	_ = w.w.Flush()
	return w.f.Close()
}

// ReadAll reads every record from path. Returns nil, nil when the file does not exist.
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) == nil {
			records = append(records, r)
		}
	}
	return records, sc.Err()
}

// DeduplicateByURL keeps only the last record per URL (by append order), preserving
// records with empty URLs as-is.
func DeduplicateByURL(records []Record) []Record {
	seen := make(map[string]int, len(records))
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if r.URL == "" {
			out = append(out, r)
			continue
		}
		if idx, exists := seen[r.URL]; exists {
			out[idx] = r // overwrite with latest occurrence
		} else {
			seen[r.URL] = len(out)
			out = append(out, r)
		}
	}
	return out
}
