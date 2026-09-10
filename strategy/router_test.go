package strategy

import (
	"testing"
	"time"
)

func mkUpstream(name string, priority, weight int) *Upstream {
	c := NewCircuit(CircuitConfig{}, false, time.Now, nil)
	c.SetAutoBan(true)
	return &Upstream{Name: name, Priority: priority, Weight: weight, Circuit: c}
}

func TestPick_PriorityTiers(t *testing.T) {
	low := mkUpstream("low-prio", 0, 1)
	high := mkUpstream("high-prio", 5, 100)
	r := NewRegistry()
	low.Models = []string{"m"}
	high.Models = []string{"m"}
	r.Replace([]*Upstream{high, low})

	for i := 0; i < 5; i++ {
		if got := r.Pick("m", nil); got != low {
			t.Fatalf("tier 0 must win while available, got %s", got.Name)
		}
	}
	low.Circuit.ManualDisable("test")
	if got := r.Pick("m", nil); got != high {
		t.Fatalf("must fall to tier 5, got %v", got)
	}
}

func TestPick_Exclude(t *testing.T) {
	a := mkUpstream("a", 0, 1)
	b := mkUpstream("b", 0, 1)
	a.Models = []string{"m"}
	b.Models = []string{"m"}
	r := NewRegistry()
	r.Replace([]*Upstream{a, b})

	first := r.Pick("m", nil)
	second := r.Pick("m", map[*Upstream]bool{first: true})
	if second == first {
		t.Fatal("exclude must be honored")
	}
	if got := r.Pick("m", map[*Upstream]bool{a: true, b: true}); got != nil {
		t.Fatal("all excluded must yield nil")
	}
}

func TestPick_SWRRObservesWeights(t *testing.T) {
	a := mkUpstream("a", 0, 10)
	b := mkUpstream("b", 0, 1)
	a.Models = []string{"m"}
	b.Models = []string{"m"}
	r := NewRegistry()
	r.Replace([]*Upstream{a, b})

	counts := map[string]int{}
	for i := 0; i < 110; i++ {
		u := r.Pick("m", nil)
		u.Circuit.ReleaseHalfOpen()
		counts[u.Name]++
	}
	if counts["a"] != 100 || counts["b"] != 10 {
		t.Fatalf("want 100/10 smooth distribution, got %v", counts)
	}
}

func TestPick_CooldownSkipped(t *testing.T) {
	a := mkUpstream("a", 0, 1)
	b := mkUpstream("b", 0, 1)
	a.Models = []string{"m"}
	b.Models = []string{"m"}
	r := NewRegistry()
	r.Replace([]*Upstream{a, b})

	a.Feed(Signal{RateLimit: &RateLimit{RetryAfter: time.Hour}})
	for i := 0; i < 5; i++ {
		if got := r.Pick("m", nil); got != b {
			t.Fatalf("cooling-down upstream must be skipped, got %s", got.Name)
		}
	}
}

func TestPick_UnknownModel(t *testing.T) {
	r := NewRegistry()
	if got := r.Pick("nope", nil); got != nil {
		t.Fatal("unknown model must yield nil")
	}
}

func TestRegistry_COWReplace(t *testing.T) {
	a := mkUpstream("a", 0, 1)
	a.Models = []string{"m"}
	r := NewRegistry()
	r.Replace([]*Upstream{a})
	old := r.Candidates("m")

	b := mkUpstream("b", 0, 1)
	b.Models = []string{"m"}
	r.Replace([]*Upstream{b})
	if len(old) != 1 || old[0] != a {
		t.Fatal("previously returned candidate slice must be unaffected")
	}
	if got := r.Pick("m", nil); got != b {
		t.Fatal("registry must serve new set")
	}
}
