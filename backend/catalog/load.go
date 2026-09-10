package catalog

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"relayd/strategy"
)

// Load reads the main config plus any credential_files, merges and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	base := filepath.Dir(path)
	for _, pattern := range cfg.CredentialFiles {
		matches, err := filepath.Glob(filepath.Join(base, pattern))
		if err != nil {
			return nil, fmt.Errorf("credential_files %q: %w", pattern, err)
		}
		sort.Strings(matches)
		for _, f := range matches {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			var part struct {
				Credentials []CredentialSpec `yaml:"credentials"`
			}
			if err := yaml.Unmarshal(b, &part); err != nil {
				return nil, fmt.Errorf("parse %s: %w", f, err)
			}
			cfg.Credentials = append(cfg.Credentials, part.Credentials...)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if len(c.AuthTokens) == 0 {
		return fmt.Errorf("auth_tokens must not be empty; refusing to start unauthenticated")
	}
	if c.Listen == "" {
		return fmt.Errorf("listen must be set")
	}
	if c.AdminListen == "" {
		return fmt.Errorf("admin_listen must be set")
	}
	if c.Listen == c.AdminListen {
		return fmt.Errorf("listen and admin_listen must differ")
	}
	if c.MaxBufferedBody <= 0 {
		c.MaxBufferedBody = 8 << 20
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = Duration(300_000_000_000) // 300s
	}
	if c.ProbeSchedule.Concurrency <= 0 {
		c.ProbeSchedule.Concurrency = 8
	}
	if c.ProbeSchedule.BudgetPerMinute <= 0 {
		c.ProbeSchedule.BudgetPerMinute = 60
	}
	if c.ProbeSchedule.Mode == "" {
		c.ProbeSchedule.Mode = "all"
	}
	if c.ProbeSchedule.Mode != "all" && c.ProbeSchedule.Mode != "passive_recovery" {
		return fmt.Errorf("probe_schedule.mode must be all or passive_recovery")
	}
	if c.ProbeSchedule.Jitter < 0 || c.ProbeSchedule.Jitter > 1 {
		return fmt.Errorf("probe_schedule.jitter must be in [0,1]")
	}

	// alias conflict: one alias must not map to two canonical names
	aliasOwner := map[string]string{}
	for canonical, m := range c.Models {
		if m.ProtocolHint != "" && m.ProtocolHint != "openai" && m.ProtocolHint != "claude" && m.ProtocolHint != "gemini" {
			return fmt.Errorf("model %s: unknown protocol_hint %q", canonical, m.ProtocolHint)
		}
		for _, a := range m.Aliases {
			if a == canonical {
				return fmt.Errorf("model %s: alias duplicates canonical name", canonical)
			}
			if owner, exists := aliasOwner[a]; exists && owner != canonical {
				return fmt.Errorf("alias %q claimed by both %q and %q", a, owner, canonical)
			}
			aliasOwner[a] = canonical
		}
	}

	seenCred := map[string]bool{}
	for i, cred := range c.Credentials {
		if cred.Name == "" {
			return fmt.Errorf("credentials[%d]: name required", i)
		}
		if seenCred[cred.Name] {
			return fmt.Errorf("duplicate credential name %q", cred.Name)
		}
		seenCred[cred.Name] = true
		p, ok := c.Providers[cred.Provider]
		if !ok {
			return fmt.Errorf("credential %q: unknown provider %q", cred.Name, cred.Provider)
		}
		if p.Protocol != "openai" && p.Protocol != "claude" && p.Protocol != "gemini" {
			return fmt.Errorf("provider %q: unknown protocol %q", cred.Provider, p.Protocol)
		}
		u, err := url.Parse(cred.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("credential %q: invalid base_url %q", cred.Name, cred.BaseURL)
		}
		if cred.APIKey == "" {
			return fmt.Errorf("credential %q: api_key required", cred.Name)
		}
		if len(cred.Models) == 0 {
			return fmt.Errorf("credential %q: models required", cred.Name)
		}
		probes := cred.Probes
		if len(probes) == 0 {
			probes = p.Probes
		}
		for j, pb := range probes {
			if pb.Type != "balance" && pb.Type != "ping" {
				return fmt.Errorf("credential %q probe[%d]: type must be balance or ping", cred.Name, j)
			}
			if pb.Interval <= 0 {
				return fmt.Errorf("credential %q probe[%d]: interval required", cred.Name, j)
			}
			if pb.Type == "balance" {
				if pb.Request == nil || pb.Request.Path == "" {
					return fmt.Errorf("credential %q probe[%d]: balance requires request.path", cred.Name, j)
				}
				if pb.Extract == nil || pb.Extract.Value == "" {
					return fmt.Errorf("credential %q probe[%d]: balance requires extract.value", cred.Name, j)
				}
			}
		}
	}
	return nil
}

// BuildUpstreams expands credentials into runtime upstreams using the catalog
// for model canonicalization. onTransition receives state-change events.
func (c *Config) BuildUpstreams(cat *Catalog, onTransition func(name string, d strategy.Decision)) ([]*strategy.Upstream, error) {
	var out []*strategy.Upstream
	for _, cred := range c.Credentials {
		prov := c.Providers[cred.Provider]

		native := map[string]string{}
		var canonicalModels []string
		for _, m := range cred.Models {
			canonical, ok := cat.Canonicalize(m)
			if !ok {
				return nil, fmt.Errorf("credential %q: model %q not in catalog (declare it under models: or its aliases)", cred.Name, m)
			}
			native[canonical] = m
			canonicalModels = append(canonicalModels, canonical)
		}

		ccfg := c.Circuit.ToStrategy()
		if prov.Circuit != nil {
			ccfg = prov.Circuit.ToStrategy()
		}
		if cred.Circuit != nil {
			ccfg = cred.Circuit.ToStrategy()
		}

		circuit := strategy.NewCircuit(ccfg, !cred.enabled(), nil, onTransition)
		circuit.SetName(cred.Name)
		circuit.SetAutoBan(cred.autoBan())

		out = append(out, &strategy.Upstream{
			Name:        cred.Name,
			Provider:    cred.Provider,
			Protocol:    prov.Protocol,
			BaseURL:     cred.BaseURL,
			APIKey:      cred.APIKey,
			Models:      canonicalModels,
			NativeModel: native,
			Priority:    cred.Priority,
			Weight:      max(cred.Weight, 1),
			Circuit:     circuit,
		})
	}
	return out, nil
}

// ProbesFor resolves the effective probe configs for a credential
// (credential overrides provider template).
func (c *Config) ProbesFor(credName string) []ProbeConfig {
	for _, cred := range c.Credentials {
		if cred.Name != credName {
			continue
		}
		if len(cred.Probes) > 0 {
			return cred.Probes
		}
		return c.Providers[cred.Provider].Probes
	}
	return nil
}
