// Package memguard provides memory-pressure throttling by polling /proc/meminfo.
// Call Throttle before dispatching new work; it blocks until available RAM is
// above both the absolute floor (256 MiB) and the fractional floor (8 % of
// total), then returns nil.  If the context is cancelled while waiting it
// returns ctx.Err().  On systems without /proc/meminfo it is a no-op.
package memguard

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// minFreeBytes is the absolute low-water mark.  New work is held back until
	// MemAvailable rises above this value.
	minFreeBytes = 256 * 1024 * 1024 // 256 MiB

	// minFreePct is the fractional low-water mark relative to MemTotal.
	minFreePct = 0.08 // 8 %

	// recheckEvery is the minimum interval between /proc/meminfo reads so that
	// calling Throttle in a tight dispatch loop stays cheap.
	recheckEvery = 2 * time.Second

	// throttleSleep is how long Throttle waits before re-sampling when memory
	// is under pressure.
	throttleSleep = 5 * time.Second
)

// Guard polls /proc/meminfo and exposes Throttle.  Zero value is usable after
// the first Throttle call; prefer New() for clarity.
type Guard struct {
	mu         sync.Mutex
	lastCheck  time.Time
	lastAvail  uint64
	lastTotal  uint64
	lastErr    error
	throttling bool // transition tracker for log-once-per-event behaviour
}

// New returns a ready-to-use Guard.
func New() *Guard { return &Guard{} }

// Throttle returns immediately when system memory is healthy.  When available
// memory is below either threshold it logs once, then sleeps and re-evaluates
// until pressure lifts or ctx is cancelled.
func (g *Guard) Throttle(ctx context.Context) error {
	for {
		avail, total, err := g.sample()
		if err != nil {
			// /proc/meminfo unreadable (non-Linux, container, etc.) — don't block.
			return nil
		}

		if !underPressure(avail, total) {
			if g.transition(false) {
				log.Printf("[memguard] memory recovered (%d MiB available) — resuming", avail>>20)
			}
			return nil
		}

		if g.transition(true) {
			log.Printf("[memguard] low memory (%d MiB available / %d MiB total) — throttling new work",
				avail>>20, total>>20)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(throttleSleep):
		}
	}
}

// transition sets the internal throttling flag and returns true when the state
// actually changed (so the caller can emit a single log line per transition).
func (g *Guard) transition(v bool) (changed bool) {
	g.mu.Lock()
	changed = g.throttling != v
	g.throttling = v
	g.mu.Unlock()
	return
}

// sample returns cached /proc/meminfo values, refreshing them at most every
// recheckEvery so Throttle calls in tight loops stay cheap.
func (g *Guard) sample() (avail, total uint64, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.lastCheck) >= recheckEvery {
		g.lastAvail, g.lastTotal, g.lastErr = readMemInfo()
		g.lastCheck = time.Now()
	}
	return g.lastAvail, g.lastTotal, g.lastErr
}

func underPressure(avail, total uint64) bool {
	if avail < minFreeBytes {
		return true
	}
	return total > 0 && float64(avail)/float64(total) < minFreePct
}

// readMemInfo parses MemTotal and MemAvailable from /proc/meminfo and returns
// them in bytes.
func readMemInfo() (avail, total uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, err = parseKBLine(line)
			if err != nil {
				return
			}
		case strings.HasPrefix(line, "MemAvailable:"):
			avail, err = parseKBLine(line)
			if err != nil {
				return
			}
		}
		if avail > 0 && total > 0 {
			return
		}
	}
	if serr := sc.Err(); serr != nil {
		return 0, 0, serr
	}
	return 0, 0, fmt.Errorf("memguard: MemAvailable/MemTotal not found in /proc/meminfo")
}

func parseKBLine(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("memguard: unexpected /proc/meminfo line: %q", line)
	}
	kb, err := strconv.ParseUint(fields[1], 10, 64)
	return kb * 1024, err
}
