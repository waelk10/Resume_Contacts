package scraper

// urlQueueCap is the maximum number of URLs the spider queue holds at once.
// New URLs discovered while the queue is full are silently dropped.
const urlQueueCap = 10_000

// urlQueue is a bounded FIFO queue for pending spider URLs.
// Push and Pop are safe for concurrent use by multiple goroutines.
type urlQueue struct {
	ch chan string
}

func newURLQueue() *urlQueue {
	return &urlQueue{ch: make(chan string, urlQueueCap)}
}

// Push enqueues url. Returns false (and drops url) if the queue is at capacity.
func (q *urlQueue) Push(url string) bool {
	select {
	case q.ch <- url:
		return true
	default:
		return false
	}
}

// Pop removes and returns the front URL.
// Returns ("", false) when the queue is empty.
func (q *urlQueue) Pop() (string, bool) {
	select {
	case u := <-q.ch:
		return u, true
	default:
		return "", false
	}
}

// Len returns the current number of URLs in the queue.
func (q *urlQueue) Len() int {
	return len(q.ch)
}

// drain removes every URL currently in the queue, appends them to dst, and
// returns the extended slice. It is not safe to call concurrently with Push.
func (q *urlQueue) drain(dst []string) []string {
	for {
		u, ok := q.Pop()
		if !ok {
			return dst
		}
		dst = append(dst, u)
	}
}
