package strategy

import "sync/atomic"

// Upstream is the shared runtime object: routing data + circuit state.
// It performs no I/O; backend layers fill the fields and drive Circuit.
type Upstream struct {
	Name     string
	Provider string
	Protocol string // openai | claude | gemini
	BaseURL  string
	APIKey   string

	// Models holds canonical model names this upstream serves.
	Models []string
	// NativeModel maps canonical -> this upstream's native model name.
	NativeModel map[string]string

	Priority int
	Weight   int

	Circuit *Circuit

	currentWeight atomic.Int64
}

func (u *Upstream) Available() bool { return u.Circuit.Available() }

func (u *Upstream) Feed(s Signal) Decision { return u.Circuit.Feed(s) }
