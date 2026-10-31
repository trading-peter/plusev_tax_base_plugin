package main

import (
	"reflect"
	"testing"
	"time"

	m "github.com/plusev-terminal/go-plugin-common/meta"
)

// TestRateLimitDeclarations pins the manifest rate limits so the declared
// budgets cannot silently drift. Numbers sourced 2026-10-31 from
// https://docs.cdp.coinbase.com/data/node/overview (free tier: 7,500 BU per
// 5 s per project, "approximately 50 requests per second").
func TestRateLimitDeclarations(t *testing.T) {
	targets := (&BaseTaxPlugin{}).GetMeta().Resources.AllowedNetworkTargets

	want := map[string][]m.RateLimitRule{
		"https://api.developer.coinbase.com/*": {
			{
				ID:       "node-rpc",
				Label:    "CDP Node RPC (free tier)",
				Limit:    50,
				WindowMs: (1 * time.Second).Milliseconds(),
				Scope:    m.RateLimitScopeCredential,
			},
		},
	}

	if len(targets) != len(want) {
		t.Fatalf("expected %d network targets, got %d: %+v", len(want), len(targets), targets)
	}

	for _, target := range targets {
		rules, ok := want[target.Pattern]
		if !ok {
			t.Fatalf("unexpected network target pattern %q", target.Pattern)
		}
		if target.TradingReserve != nil {
			t.Errorf("%s: expected no TradingReserve, got %v", target.Pattern, *target.TradingReserve)
		}
		got := target.Limits
		if len(got) != len(rules) {
			t.Fatalf("%s: expected %d rules, got %d: %+v", target.Pattern, len(rules), len(got), got)
		}
		for i, exp := range rules {
			if !reflect.DeepEqual(got[i], exp) {
				t.Errorf("%s rule %d: expected %+v, got %+v", target.Pattern, i, exp, got[i])
			}
		}
	}
}

// TestGetRateLimitsOptsOutOfLegacyLayer guards the explicit opt-out: manifest
// limits are declared above, so the legacy per-command limiter must be empty
// to avoid double throttling.
func TestGetRateLimitsOptsOutOfLegacyLayer(t *testing.T) {
	limits := (&BaseTaxPlugin{}).GetRateLimits()
	if limits == nil {
		t.Fatal("GetRateLimits must return an empty slice, not nil, to keep the export")
	}
	if len(limits) != 0 {
		t.Fatalf("expected no legacy rate limits, got %+v", limits)
	}
}
