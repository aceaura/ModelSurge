package strategy

import "time"

type Kind uint8

const (
	KindBalance Kind = iota
	KindPing
	KindTraffic
)

func (k Kind) String() string {
	switch k {
	case KindBalance:
		return "balance"
	case KindPing:
		return "ping"
	case KindTraffic:
		return "traffic"
	}
	return "unknown"
}

type Signal struct {
	Kind      Kind
	At        time.Time
	OK        bool
	Fatal     bool
	RateLimit *RateLimit
	Balance   *float64
	Latency   time.Duration
	Reason    string
}

type RateLimit struct {
	RetryAfter time.Duration
	Source     string
}

type Decision struct {
	From    Status
	To      Status
	Changed bool
	Reason  string
}
