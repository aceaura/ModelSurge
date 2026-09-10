package obs

import (
	"sync"
	"time"
)

type Event struct {
	Seq      uint64    `json:"seq"`
	At       time.Time `json:"at"`
	Upstream string    `json:"upstream"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Reason   string    `json:"reason"`
	Balance  *float64  `json:"balance,omitempty"`
}

// Ring is a fixed-size in-memory event buffer with monotonic seq numbers.
type Ring struct {
	mu   sync.Mutex
	buf  []Event
	next uint64
	size int
}

func NewRing(size int) *Ring {
	if size <= 0 {
		size = 1024
	}
	return &Ring{buf: make([]Event, size), size: size}
}

func (r *Ring) Add(e Event) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	e.Seq = r.next
	e.At = time.Now()
	r.buf[int(r.next%uint64(r.size))] = e
	return r.next
}

// Since returns events with seq > since, oldest first.
func (r *Ring) Since(since uint64) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for i := uint64(1); i <= uint64(r.size) && i <= r.next; i++ {
		e := r.buf[int((r.next-i+1)%uint64(r.size))]
		out = append(out, e)
	}
	// out is newest-first; reverse and filter
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	var filtered []Event
	for _, e := range out {
		if e.Seq > since {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (r *Ring) ForUpstream(name string, limit int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for i := uint64(0); i < uint64(r.size) && i < r.next; i++ {
		e := r.buf[int((r.next-i)%uint64(r.size))]
		if e.Upstream == name {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
