package basescan

// AddressTx is a single transaction entry returned by cdp_listAddressTransactions.
type AddressTx struct {
	Name        string     `json:"name"`
	Hash        string     `json:"hash"`
	BlockHash   string     `json:"blockHash"`
	BlockHeight string     `json:"blockHeight"`
	Status      string     `json:"status"`
	Ethereum    EthContent `json:"ethereum"`
}

// EthContent is the ethereum-format transaction data returned by CDP Node.
type EthContent struct {
	From            string          `json:"from"`
	To              string          `json:"to"`
	Value           string          `json:"value"`
	Hash            string          `json:"hash"`
	BlockNumber     string          `json:"blockNumber"`
	BlockTimestamp  string          `json:"blockTimestamp"`
	Receipt         TxReceipt       `json:"receipt"`
	TokenTransfers  []TokenTransfer `json:"tokenTransfers"`
	FlattenedTraces []Trace         `json:"flattenedTraces"`
}

// TxReceipt holds gas usage and execution status.
type TxReceipt struct {
	Status            string `json:"status"`            // "1" = success, "0" = failed
	GasUsed           string `json:"gasUsed"`           // decimal string, gas units
	EffectiveGasPrice string `json:"effectiveGasPrice"` // decimal string, wei per gas
}

// TokenTransfer is a single ERC-20 Transfer event captured in the transaction.
type TokenTransfer struct {
	TokenAddress string `json:"tokenAddress"`
	FromAddress  string `json:"fromAddress"`
	ToAddress    string `json:"toAddress"`
	Value        string `json:"value"` // raw token units (no decimals applied)
}

// Trace is a flattened EVM internal call trace entry.
type Trace struct {
	TraceType string `json:"traceType"`
	CallType  string `json:"callType"`
	From      string `json:"from"`
	To        string `json:"to"`
	Value     string `json:"value"`  // raw wei
	Status    string `json:"status"` // "1" = success
}

// listResponse is the JSON-RPC result envelope for cdp_listAddressTransactions.
// ErrCode/ErrMessage are populated when CDP returns a gRPC-level error as the
// result object (e.g. code=8 "message larger than max").
type listResponse struct {
	AddressTransactions []AddressTx `json:"addressTransactions"`
	NextPageToken       string      `json:"nextPageToken"`
	ErrCode             int         `json:"code"`
	ErrMessage          string      `json:"message"`
}

// rpcResponse is the top-level JSON-RPC response envelope.
type rpcResponse[T any] struct {
	ID      int       `json:"id"`
	JSONRPC string    `json:"jsonrpc"`
	Result  T         `json:"result"`
	Error   *rpcError `json:"error,omitempty"`
}

// rpcError represents a JSON-RPC error.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
