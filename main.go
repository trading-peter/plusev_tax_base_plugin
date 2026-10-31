package main

import (
	"time"

	"github.com/plusev-terminal/go-plugin-common/logging"
	m "github.com/plusev-terminal/go-plugin-common/meta"
	"github.com/plusev-terminal/go-plugin-common/plugin"
	"github.com/plusev-terminal/go-plugin-common/requester"
	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/trading-peter/plusev_tax_base_plugin/basescan"
)

// ============================================================================
// Main - Register the plugin
// ============================================================================

func init() {
	p := &BaseTaxPlugin{}
	plugin.RegisterPlugin(p)
}

var log = logging.NewLogger("base-basescan-import")

func main() {}

// ============================================================================
// Plugin Implementation
// ============================================================================

// BaseTaxPlugin implements the plugin.Plugin interface for importing
// Base chain transaction history via the Coinbase Developer Platform Node API.
type BaseTaxPlugin struct {
	config *plugin.ConfigStore
	client *basescan.Client
}

// GetMeta returns the plugin metadata.
func (p *BaseTaxPlugin) GetMeta() m.Meta {
	return m.Meta{
		PluginID:    "cdp-base-import",
		Name:        "Base CDP Importer",
		AppID:       "plusev_taxes",
		Category:    "Import",
		Description: "Imports transfers and trades from a Base wallet using the Coinbase Developer Platform Node API",
		Author:      "trading_peter",
		Version:     "v1.0.0",
		Tags:        []string{"base", "cdp", "evm", "crypto", "tax", "import"},
		Resources: m.ResourceAccess{
			AllowedNetworkTargets: []m.NetworkTargetRule{
				{
					Pattern: "https://api.developer.coinbase.com/*",
					// All traffic is CDP Node JSON-RPC on /rpc/v1/base/{API_KEY}.
					// Free tier: 7,500 billing units per 5 s per project; the docs
					// convert that to "approximately 50 requests per second"
					// (average call = 30 BU). Declared as a flat per-API-key rule
					// because requests carry no Cost; conservative for the heavier
					// cdp_listAddressTransactions/eth_call methods.
					// Sources (retrieved 2026-10-31):
					//   - https://docs.cdp.coinbase.com/data/node/overview (Pricing & Rate Limits)
					//   - https://docs.cdp.coinbase.com/api-reference/networks (JSON-RPC API)
					Limits: []m.RateLimitRule{
						// 50 rps free tier per project/API key: https://docs.cdp.coinbase.com/data/node/overview
						m.NewRateLimitRule("node-rpc", 50, time.Second).
							WithLabel("CDP Node RPC (free tier)").
							PerCredential(),
					},
				},
			},
		},
		Features: []string{},
	}
}

// GetRateLimits intentionally returns no legacy per-command limits: the CDP
// Node API budget is declared as manifest Limits on the network target above,
// and an empty slice opts out of the legacy layer so requests are not
// double-throttled.
func (p *BaseTaxPlugin) GetRateLimits() []plugin.RateLimit {
	return []plugin.RateLimit{}
}

// GetConfigFields returns the configuration fields needed by this plugin.
func (p *BaseTaxPlugin) GetConfigFields() []plugin.ConfigField {
	if p.client == nil {
		p.client = basescan.NewClient(requester.NewRequester())
	}

	return p.client.GetConfigFields()
}

// OnInit is called when the plugin is initialized with user configuration.
func (p *BaseTaxPlugin) OnInit(config *plugin.ConfigStore) error {
	p.config = config

	p.client = basescan.NewClient(requester.NewRequester())

	credentials := make(map[string]string)

	if apiKey := config.GetString("apiKey"); apiKey != "" {
		credentials["apiKey"] = apiKey
	}

	if walletAddress := config.GetString("walletAddress"); walletAddress != "" {
		credentials["walletAddress"] = walletAddress
	}

	p.client.SetCredentials(credentials)

	return nil
}

// OnShutdown is called when the plugin is being shut down.
func (p *BaseTaxPlugin) OnShutdown() error {
	return nil
}

// RegisterCommands registers all command handlers.
func (p *BaseTaxPlugin) RegisterCommands(router *plugin.CommandRouter) {
	router.Register(tax.CMD_START_IMPORT, p.handleStartImport)
}
