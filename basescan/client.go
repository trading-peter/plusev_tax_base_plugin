package basescan

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/plusev-terminal/go-plugin-common/logging"
	"github.com/plusev-terminal/go-plugin-common/plugin"
	rt "github.com/plusev-terminal/go-plugin-common/requester/types"
)

const (
	// cdpRPCBase is the CDP Node RPC base URL. The Client API Key is appended as a
	// path segment: https://api.developer.coinbase.com/rpc/v1/base/{CLIENT_API_KEY}
	cdpRPCBase = "https://api.developer.coinbase.com/rpc/v1/base"

	// defaultPageSize is the starting number of transactions per CDP page.
	// CDP always returns full trace calldata (no field filtering), so complex DeFi
	// or NFT-batch txs can be 1–2 MB each. The CDP gRPC message cap is 4 MB.
	// FetchTransactions will automatically halve this on size errors.
	defaultPageSize = 5
)

// Client is the CDP Node API client for fetching Base chain transaction history.
type Client struct {
	req        rt.RequestDoer
	log        *logging.Logger
	apiKey     string
	walletAddr string
}

// NewClient creates a new CDP Node API client.
func NewClient(req rt.RequestDoer) *Client {
	return &Client{
		req: req,
		log: logging.NewLogger("base-cdp-import"),
	}
}

// SetCredentials stores the API key and wallet address.
func (c *Client) SetCredentials(creds map[string]string) {
	if v, ok := creds["apiKey"]; ok {
		c.apiKey = v
	}

	if v, ok := creds["walletAddress"]; ok {
		c.walletAddr = strings.ToLower(v)
	}
}

// WalletAddress returns the configured wallet address (lowercase).
func (c *Client) WalletAddress() string {
	return c.walletAddr
}

// GetConfigFields returns the configuration fields required by this plugin.
func (c *Client) GetConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Name:        "apiKey",
			Label:       "CDP Client API Key",
			Type:        "secret",
			Required:    true,
			Encrypt:     true,
			Mask:        true,
			Description: "Your Coinbase Developer Platform Client API Key. Get one at https://portal.cdp.coinbase.com/projects/api-keys",
		},
		{
			Name:        "walletAddress",
			Label:       "Base Wallet Address",
			Type:        "text",
			Required:    true,
			Description: "The Base (EVM) wallet address to import transactions for (0x...)",
		},
	}
}

// rpcRequest is the JSON-RPC request body.
type rpcRequest struct {
	ID      int           `json:"id"`
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// call sends a JSON-RPC request and decodes the response into result.
func (c *Client) call(method string, params []interface{}, result interface{}) error {
	endpoint := fmt.Sprintf("%s/%s", cdpRPCBase, c.apiKey)

	body := rpcRequest{
		ID:      1,
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cdp marshal: %w", err)
	}

	req := &rt.Request{
		URL:    endpoint,
		Method: "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: payload,
	}

	resp, err := c.req.Send(req, nil)
	if err != nil {
		return fmt.Errorf("cdp send: %w", err)
	}

	if resp.Error != "" {
		return fmt.Errorf("cdp http error: %s", resp.Error)
	}

	if resp.Status >= 400 {
		preview := string(resp.Body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return fmt.Errorf("cdp http status %d: %s", resp.Status, preview)
	}

	c.log.DebugWithData("cdp response", map[string]any{
		"method":  method,
		"status":  resp.Status,
		"bodyLen": len(resp.Body),
		"bodyPrefix": func() string {
			if len(resp.Body) > 120 {
				return string(resp.Body[:120])
			}
			return string(resp.Body)
		}(),
	})

	if err := json.NewDecoder(bytes.NewReader(resp.Body)).Decode(result); err != nil {
		return fmt.Errorf("cdp decode: %w", err)
	}

	return nil
}

// FetchTransactions calls cdp_listAddressTransactions with cursor-based pagination
// and invokes cb for each page of results. Stops if cb returns an error.
// fetchPage fetches a single page with the given pageToken and pageSize.
// If CDP returns a gRPC "message larger than max" error (code 8 in the result
// object), it retries with progressively smaller page sizes down to 1.
func (c *Client) fetchPage(pageToken string, pageSize int) ([]AddressTx, string, error) {
	for pageSize >= 1 {
		params := map[string]interface{}{
			"address":  c.walletAddr,
			"pageSize": pageSize,
		}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}

		var rpc rpcResponse[listResponse]
		if err := c.call("cdp_listAddressTransactions", []interface{}{params}, &rpc); err != nil {
			return nil, "", fmt.Errorf("fetch transactions: %w", err)
		}

		if rpc.Error != nil {
			return nil, "", fmt.Errorf("cdp rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
		}

		// CDP returns gRPC-level errors as the result object (code + message fields).
		// Code 8 = RESOURCE_EXHAUSTED = response too large for gRPC message limit.
		if rpc.Result.ErrCode != 0 {
			if pageSize == 1 {
				return nil, "", fmt.Errorf("cdp result error %d: %s", rpc.Result.ErrCode, rpc.Result.ErrMessage)
			}
			next := pageSize / 2
			c.log.DebugWithData("cdp page too large, retrying smaller", map[string]any{
				"errorCode": rpc.Result.ErrCode,
				"from":      pageSize,
				"to":        next,
			})
			pageSize = next
			continue
		}

		c.log.DebugWithData("cdp_listAddressTransactions page", map[string]any{
			"address":  c.walletAddr,
			"pageSize": pageSize,
			"count":    len(rpc.Result.AddressTransactions),
			"hasNext":  rpc.Result.NextPageToken != "",
		})

		return rpc.Result.AddressTransactions, rpc.Result.NextPageToken, nil
	}
	return nil, "", fmt.Errorf("cdp page size reached 0")
}

// FetchTransactions calls cdp_listAddressTransactions with cursor-based pagination
// and invokes cb for each page of results. Stops if cb returns an error.
// Page size adapts automatically if CDP returns a gRPC message-too-large error.
func (c *Client) FetchTransactions(cb func([]AddressTx) error) error {
	pageToken := ""
	pageSize := defaultPageSize

	for {
		txs, nextToken, err := c.fetchPage(pageToken, pageSize)
		if err != nil {
			return err
		}

		if len(txs) == 0 {
			break
		}

		if err := cb(txs); err != nil {
			return err
		}

		if nextToken == "" {
			break
		}

		pageToken = nextToken
	}

	return nil
}

// TokenInfo holds on-chain metadata for an ERC-20 token.
type TokenInfo struct {
	Symbol   string
	Decimals int
}

// GetTokenInfo fetches the ERC-20 symbol and decimals for a token contract via eth_call.
// On any error a fallback of 18 decimals and a truncated address as symbol is returned.
func (c *Client) GetTokenInfo(contractAddr string) TokenInfo {
	fallback := TokenInfo{Decimals: 18, Symbol: contractAddr}
	if len(contractAddr) >= 10 {
		fallback.Symbol = contractAddr[:10]
	}

	// Fetch decimals() — selector 0x313ce567.
	dec := 18
	var decRPC rpcResponse[string]
	if err := c.call("eth_call", []interface{}{
		map[string]string{"to": contractAddr, "data": "0x313ce567"},
		"latest",
	}, &decRPC); err == nil && decRPC.Error == nil {
		if d, err := parseABIUint(decRPC.Result); err == nil {
			dec = d
		}
	}

	// Fetch symbol() — selector 0x95d89b41.
	var symRPC rpcResponse[string]
	if err := c.call("eth_call", []interface{}{
		map[string]string{"to": contractAddr, "data": "0x95d89b41"},
		"latest",
	}, &symRPC); err == nil && symRPC.Error == nil {
		if sym, err := parseABIString(symRPC.Result); err == nil && sym != "" {
			return TokenInfo{Symbol: sym, Decimals: dec}
		}
	}

	fallback.Decimals = dec
	return fallback
}

// parseABIUint decodes a 32-byte right-aligned ABI uint (e.g. decimals()) from hex.
func parseABIUint(hexStr string) (int, error) {
	h := strings.TrimPrefix(hexStr, "0x")
	if len(h) == 0 {
		return 0, fmt.Errorf("empty result")
	}
	// Pad to at least 2 chars; last 2 hex chars = uint8 value.
	for len(h) < 2 {
		h = "0" + h
	}
	n, err := strconv.ParseInt(h[len(h)-2:], 16, 64)
	return int(n), err
}

// parseABIString decodes an ABI-encoded string (or bytes32 fallback) from a hex result.
func parseABIString(hexStr string) (string, error) {
	h := strings.TrimPrefix(hexStr, "0x")
	if len(h) == 0 {
		return "", fmt.Errorf("empty result")
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return "", err
	}
	// Standard ABI string: bytes 0–31 = offset, bytes 32–63 = length, then data.
	if len(b) >= 96 {
		var length int64
		for _, byt := range b[32:64] {
			length = length<<8 | int64(byt)
		}
		if length > 0 && 64+int(length) <= len(b) {
			return strings.TrimRight(string(b[64:64+length]), "\x00"), nil
		}
	}
	// Fallback: bytes32 encoding used by some non-standard tokens.
	return strings.TrimRight(string(b), "\x00"), nil
}
