package probe

import (
	"context"
	"time"

	"relayd/backend/catalog"
	"relayd/strategy"
)

type balanceProber struct {
	cfg catalog.ProbeConfig
}

func (p *balanceProber) Name() string            { return "balance" }
func (p *balanceProber) Interval() time.Duration { return p.cfg.Interval.D() }

func (p *balanceProber) Probe(ctx context.Context, t Target) strategy.Signal {
	sig := strategy.Signal{Kind: strategy.KindBalance, At: time.Now()}

	body, err := doProbeRequest(ctx, t.Client, t.BaseURL, t.APIKey, *p.cfg.Request)
	if err != nil {
		// conservative: probe failure never changes upstream state
		sig.OK = false
		sig.Reason = "balance query failed: " + err.Error()
		return sig
	}
	value, err := extractValue(body, *p.cfg.Extract)
	if err != nil {
		sig.OK = false
		sig.Reason = "balance extract failed: " + err.Error()
		return sig
	}

	if p.cfg.Minus != nil {
		body2, err := doProbeRequest(ctx, t.Client, t.BaseURL, t.APIKey, p.cfg.Minus.Request)
		if err != nil {
			sig.OK = false
			sig.Reason = "balance minus query failed: " + err.Error()
			return sig
		}
		sub, err := extractValue(body2, p.cfg.Minus.Extract)
		if err != nil {
			sig.OK = false
			sig.Reason = "balance minus extract failed: " + err.Error()
			return sig
		}
		value -= sub
	}

	sig.OK = true
	sig.Balance = &value
	if p.cfg.Rule != nil && value <= p.cfg.Rule.DisableBelow {
		sig.Fatal = true
		sig.Reason = "balance below threshold"
	}
	return sig
}
