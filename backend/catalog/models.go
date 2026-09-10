package catalog

import "relayd/strategy"

// Catalog holds canonical model metadata and alias mapping.
type Catalog struct {
	canonical map[string]string // alias or canonical -> canonical
	protocol  map[string]string // canonical -> protocol hint
}

func NewCatalog(models map[string]ModelSpec) *Catalog {
	c := &Catalog{
		canonical: map[string]string{},
		protocol:  map[string]string{},
	}
	for canonical, spec := range models {
		c.canonical[canonical] = canonical
		c.protocol[canonical] = spec.ProtocolHint
		for _, a := range spec.Aliases {
			c.canonical[a] = canonical
		}
	}
	return c
}

func (c *Catalog) Canonicalize(name string) (string, bool) {
	v, ok := c.canonical[name]
	return v, ok
}

func (c *Catalog) ProtocolHint(canonical string) string {
	return c.protocol[canonical]
}

// NativeFor resolves the upstream-specific native name for a canonical model.
func (c *Catalog) NativeFor(u *strategy.Upstream, canonical string) (string, bool) {
	v, ok := u.NativeModel[canonical]
	return v, ok
}

// CanonicalModels lists all canonical names (unsorted; callers sort if needed).
func (c *Catalog) CanonicalModels() []string {
	out := make([]string, 0, len(c.protocol))
	for name := range c.protocol {
		out = append(out, name)
	}
	return out
}
