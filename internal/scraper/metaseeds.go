package scraper

import (
	"bufio"
	"math/rand"
	"os"
	"strings"
	"sync"
)

const (
	metaSeedsFile = "meta-seeds.txt"
	metaSeedsCap  = 2048
)

// metaSeedStore holds search URLs discovered during crawl cycles. It is
// persisted to disk so discoveries survive across process restarts.
// When the store reaches metaSeedsCap, the oldest entry is evicted to make
// room for the newest discovery (ring-buffer semantics).
type metaSeedStore struct {
	mu   sync.Mutex
	urls []string
	seen map[string]bool
}

func newMetaSeedStore() *metaSeedStore {
	return &metaSeedStore{seen: make(map[string]bool)}
}

// load replaces the in-memory store from the persisted file.
// A missing file is silently ignored — normal on first run.
func (s *metaSeedStore) load() {
	f, err := os.Open(metaSeedsFile)
	if err != nil {
		return
	}
	defer f.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.urls = nil // nil instead of [:0] so the backing array and its string pointers are released
	s.seen = make(map[string]bool)

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		u := strings.TrimSpace(sc.Text())
		if u == "" || s.seen[u] {
			continue
		}
		s.seen[u] = true
		s.urls = append(s.urls, u)
	}
}

// add records u if not already present. When at capacity, the oldest entry is
// evicted so fresh discoveries always have room.
func (s *metaSeedStore) add(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[u] {
		return
	}
	if len(s.urls) >= metaSeedsCap {
		evicted := s.urls[0]
		delete(s.seen, evicted)
		s.urls[0] = "" // zero before reslice so the evicted string data can be GC'd
		s.urls = s.urls[1:]
	}
	s.seen[u] = true
	s.urls = append(s.urls, u)
}

// all returns a shuffled copy of all stored URLs.
func (s *metaSeedStore) all() []string {
	s.mu.Lock()
	cp := make([]string, len(s.urls))
	copy(cp, s.urls)
	s.mu.Unlock()
	rand.Shuffle(len(cp), func(i, j int) { cp[i], cp[j] = cp[j], cp[i] })
	return cp
}

// flush atomically overwrites the persisted file with the current in-memory
// store. Errors are silently dropped — meta-seeds are best-effort.
func (s *metaSeedStore) flush() {
	s.mu.Lock()
	cp := make([]string, len(s.urls))
	copy(cp, s.urls)
	s.mu.Unlock()

	f, err := os.CreateTemp(".", ".meta-seeds-*.tmp")
	if err != nil {
		return
	}
	tmpName := f.Name()
	w := bufio.NewWriter(f)
	for _, u := range cp {
		_, _ = w.WriteString(u + "\n")
	}
	if err := w.Flush(); err != nil {
		f.Close()
		_ = os.Remove(tmpName)
		return
	}
	f.Close()
	_ = os.Rename(tmpName, metaSeedsFile)
}
