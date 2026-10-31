package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plusev-terminal/go-plugin-common/plugin"
	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/shopspring/decimal"
	"github.com/trading-peter/plusev_tax_base_plugin/basescan"
)

const (
	ethDecimals = 18 // ETH (and all EVM native gas tokens) use 18 decimals

	// wethAddress is the Wrapped ETH contract on Base. Wrap and unwrap transactions
	// are neutral conversions and should not be recorded as taxable events.
	wethAddress = "0x4200000000000000000000000000000000000006"
)

// cdpTxBundle represents a CDP transaction with extracted net flows per token/ETH.
type cdpTxBundle struct {
	Hash        string
	BlockHeight uint64
	Time        time.Time
	ETHNet      decimal.Decimal // positive = received, negative = sent
	GasFeePaid  decimal.Decimal // always non-negative, only set if wallet is tx sender
	TokenFlows  map[string]*tokenMove
}

// tokenMove represents a net token flow for the wallet in a single transaction.
type tokenMove struct {
	Symbol   string
	Contract string
	Net      decimal.Decimal
	Decimals int
}

// handleStartImport fetches all Base chain transactions from CDP Node and submits
// them as trades or transfers depending on the detected transaction pattern.
func (p *BaseTaxPlugin) handleStartImport(params map[string]any) plugin.Response {
	if p.client == nil {
		return plugin.ErrorResponseMsg("client not initialized")
	}

	walletAddr := p.client.WalletAddress()
	if walletAddr == "" {
		return plugin.ErrorResponseMsg("walletAddress is required")
	}

	importParams := tax.StartImportParamsFromMap(params)
	fromTime := importParams.ResolveFrom()
	toTime := importParams.ResolveTo()
	account := importParams.Account

	log.InfoWithData("Starting Base import", map[string]any{
		"wallet": walletAddr,
		"from":   fromTime,
		"to":     toTime,
	})

	// tokenCache caches on-chain ERC-20 metadata to avoid redundant eth_call round-trips.
	tokenCache := map[string]basescan.TokenInfo{}

	// bundles accumulates all transactions keyed by their tx hash.
	bundles := map[string]*cdpTxBundle{}

	wallet := strings.ToLower(walletAddr)

	if err := p.client.FetchTransactions(func(txs []basescan.AddressTx) error {
		for i := range txs {
			tx := &txs[i]

			// Only process confirmed, successful transactions.
			if tx.Status != "CONFIRMED" {
				continue
			}
			if tx.Ethereum.Receipt.Status != "1" {
				continue
			}

			// Parse block height from the ethereum sub-object (decimal string).
			height, err := strconv.ParseUint(tx.Ethereum.BlockNumber, 10, 64)
			if err != nil {
				log.WarnWithData("Failed to parse block number, skipping tx", map[string]any{
					"hash":        tx.Hash,
					"blockNumber": tx.Ethereum.BlockNumber,
				})
				continue
			}

			// Parse timestamp directly from the ethereum sub-object (RFC3339).
			ts, tsErr := time.Parse(time.RFC3339, tx.Ethereum.BlockTimestamp)
			if tsErr != nil {
				ts = time.Time{}
			}

			b := getOrCreateCDPBundle(bundles, tx.Hash, height)
			if !ts.IsZero() {
				b.Time = ts
			}

			// Gas fee — only charged to the wallet when it is the transaction sender.
			if strings.EqualFold(tx.Ethereum.From, wallet) {
				gasUsed, _ := decimal.NewFromString(tx.Ethereum.Receipt.GasUsed)
				gasPrice, _ := decimal.NewFromString(tx.Ethereum.Receipt.EffectiveGasPrice)
				b.GasFeePaid = gasUsed.Mul(gasPrice).Shift(-ethDecimals)
			}

			// ETH net flow from internal CALL traces (covers direct sends and DEX ETH paths).
			for _, trace := range tx.Ethereum.FlattenedTraces {
				if trace.Status != "1" {
					continue
				}
				if !strings.EqualFold(trace.TraceType, "CALL") && !strings.EqualFold(trace.CallType, "CALL") {
					continue
				}
				if trace.Value == "" || trace.Value == "0" {
					continue
				}
				v, err := decimal.NewFromString(trace.Value)
				if err != nil {
					continue
				}
				if strings.EqualFold(trace.From, wallet) {
					b.ETHNet = b.ETHNet.Sub(v.Shift(-ethDecimals))
				}
				if strings.EqualFold(trace.To, wallet) {
					b.ETHNet = b.ETHNet.Add(v.Shift(-ethDecimals))
				}
			}

			// Token flows from ERC-20 Transfer logs.
			for _, tt := range tx.Ethereum.TokenTransfers {
				isFrom := strings.EqualFold(tt.FromAddress, wallet)
				isTo := strings.EqualFold(tt.ToAddress, wallet)
				if !isFrom && !isTo {
					continue
				}

				contract := strings.ToLower(tt.TokenAddress)

				// Fetch and cache token metadata.
				info, ok := tokenCache[contract]
				if !ok {
					info = p.client.GetTokenInfo(contract)
					tokenCache[contract] = info
				}

				v, err := decimal.NewFromString(tt.Value)
				if err != nil {
					continue
				}
				tokenAmt := v.Shift(-int32(info.Decimals))

				if _, ok := b.TokenFlows[contract]; !ok {
					b.TokenFlows[contract] = &tokenMove{
						Symbol:   info.Symbol,
						Contract: contract,
						Decimals: info.Decimals,
					}
				}
				if isTo {
					b.TokenFlows[contract].Net = b.TokenFlows[contract].Net.Add(tokenAmt)
				} else {
					b.TokenFlows[contract].Net = b.TokenFlows[contract].Net.Sub(tokenAmt)
				}
			}
		}

		return nil
	}); err != nil {
		return plugin.ErrorResponse(fmt.Errorf("failed to fetch transactions: %w", err))
	}

	// Sort bundles chronologically before processing.
	sortedBundles := make([]*cdpTxBundle, 0, len(bundles))
	for _, b := range bundles {
		sortedBundles = append(sortedBundles, b)
	}

	sort.Slice(sortedBundles, func(i, j int) bool {
		return sortedBundles[i].BlockHeight < sortedBundles[j].BlockHeight
	})

	tradeCount := 0
	transferCount := 0
	skippedCount := 0
	timeFilteredCount := 0

	for _, bundle := range sortedBundles {
		// Apply time filter.
		if !fromTime.IsZero() && !bundle.Time.IsZero() && bundle.Time.Before(fromTime) {
			timeFilteredCount++
			continue
		}

		if !toTime.IsZero() && !bundle.Time.IsZero() && bundle.Time.After(toTime) {
			timeFilteredCount++
			continue
		}

		trades, transfers, skipped, err := processCDPBundle(bundle, walletAddr, account)
		if err != nil {
			return plugin.ErrorResponse(err)
		}

		tradeCount += trades
		transferCount += transfers
		skippedCount += skipped
	}

	log.InfoWithData("Base import finished", map[string]any{
		"trades":         tradeCount,
		"transfers":      transferCount,
		"totalSubmitted": tradeCount + transferCount,
		"skippedRecords": skippedCount,
		"timeFiltered":   timeFilteredCount,
	})

	return plugin.SuccessResponse(map[string]any{
		"trades":         tradeCount,
		"transfers":      transferCount,
		"totalSubmitted": tradeCount + transferCount,
		"skippedRecords": skippedCount,
		"timeFiltered":   timeFilteredCount,
	})
}

// getOrCreateCDPBundle returns the existing bundle for hash or creates a new one.
func getOrCreateCDPBundle(bundles map[string]*cdpTxBundle, hash string, height uint64) *cdpTxBundle {
	if b, ok := bundles[hash]; ok {
		return b
	}

	b := &cdpTxBundle{
		Hash:        hash,
		BlockHeight: height,
		TokenFlows:  map[string]*tokenMove{},
	}

	bundles[hash] = b
	return b
}

// processCDPBundle classifies a transaction bundle and submits the appropriate
// trade or transfer records. Returns (trades, transfers, skipped, error).
func processCDPBundle(bundle *cdpTxBundle, walletAddr, account string) (int, int, int, error) {
	tsStr := ""
	if !bundle.Time.IsZero() {
		tsStr = bundle.Time.Format(time.RFC3339)
	}

	// Build tokensIn and tokensOut from net token flows.
	var tokensIn []tokenMoveOut
	var tokensOut []tokenMoveOut

	for _, flow := range bundle.TokenFlows {
		if flow.Net.IsPositive() {
			tokensIn = append(tokensIn, tokenMoveOut{
				Symbol:   flow.Symbol,
				Contract: flow.Contract,
				Amount:   flow.Net,
				Decimals: flow.Decimals,
			})
		} else if flow.Net.IsNegative() {
			tokensOut = append(tokensOut, tokenMoveOut{
				Symbol:   flow.Symbol,
				Contract: flow.Contract,
				Amount:   flow.Net.Neg(),
				Decimals: flow.Decimals,
			})
		}
	}

	ethIn := decimal.Zero
	ethOut := decimal.Zero
	if bundle.ETHNet.IsPositive() {
		ethIn = bundle.ETHNet
	} else if bundle.ETHNet.IsNegative() {
		ethOut = bundle.ETHNet.Neg()
	}

	feeAmount := ""
	if bundle.GasFeePaid.IsPositive() {
		feeAmount = bundle.GasFeePaid.String()
	}

	hasTokenIn := len(tokensIn) > 0
	hasTokenOut := len(tokensOut) > 0
	hasEthIn := ethIn.IsPositive()
	hasEthOut := ethOut.IsPositive()

	// Detect WETH wrap/unwrap: ETH → WETH (deposit) or WETH → ETH (withdrawal).
	if (hasEthOut && len(tokensIn) == 1 && strings.EqualFold(tokensIn[0].Contract, wethAddress)) ||
		(hasEthIn && len(tokensOut) == 1 && strings.EqualFold(tokensOut[0].Contract, wethAddress)) {
		log.DebugWithData("Skipping WETH wrap/unwrap", map[string]any{"hash": bundle.Hash})
		return 0, 0, 0, nil
	}

	switch {
	case hasTokenIn && hasTokenOut:
		// Token → Token swap.
		return submitTrade(bundle.Hash, tsStr, account, feeAmount,
			tokensOut[0],
			tokensIn[0],
		)

	case hasEthOut && hasTokenIn:
		// ETH → Token swap.
		ethMove := tokenMoveOut{Symbol: "ETH", Decimals: ethDecimals, Amount: ethOut}
		return submitTrade(bundle.Hash, tsStr, account, feeAmount, ethMove, tokensIn[0])

	case hasTokenOut && hasEthIn:
		// Token → ETH swap.
		ethMove := tokenMoveOut{Symbol: "ETH", Decimals: ethDecimals, Amount: ethIn}
		return submitTrade(bundle.Hash, tsStr, account, feeAmount, tokensOut[0], ethMove)

	case hasEthIn && !hasEthOut && !hasTokenIn && !hasTokenOut:
		// Pure ETH deposit.
		transfer := tax.PluginTransfer{
			TxID:          bundle.Hash,
			Ts:            tsStr,
			Account:       account,
			Asset:         "ETH",
			AssetDecimals: int32(ethDecimals),
			Amount:        ethIn.String(),
			Action:        "DEPOSIT",
		}
		return submitTransfer(bundle.Hash, transfer)

	case hasEthOut && !hasEthIn && !hasTokenIn && !hasTokenOut:
		// Pure ETH withdrawal.
		transfer := tax.PluginTransfer{
			TxID:          bundle.Hash,
			Ts:            tsStr,
			Account:       account,
			Asset:         "ETH",
			AssetDecimals: int32(ethDecimals),
			Amount:        ethOut.String(),
			Action:        "WITHDRAWAL",
			Fee:           feeAmount,
			FeeCurrency:   "ETH",
			FeeDecimals:   int32(ethDecimals),
		}
		return submitTransfer(bundle.Hash, transfer)

	case hasTokenIn && !hasTokenOut && !hasEthIn && !hasEthOut:
		// Token deposit(s).
		return submitTokenTransfers(bundle.Hash, tsStr, account, "", walletAddr, tokensIn, "DEPOSIT", feeAmount)

	case hasTokenOut && !hasTokenIn && !hasEthIn && !hasEthOut:
		// Token withdrawal(s).
		return submitTokenTransfers(bundle.Hash, tsStr, account, walletAddr, "", tokensOut, "WITHDRAWAL", feeAmount)

	default:
		log.DebugWithData("Skipping unrecognized tx pattern", map[string]any{
			"hash":        bundle.Hash,
			"hasEthIn":    hasEthIn,
			"hasEthOut":   hasEthOut,
			"hasTokenIn":  hasTokenIn,
			"hasTokenOut": hasTokenOut,
		})
		return 0, 0, 1, nil
	}
}

// tokenMoveOut is used for submission helpers (renamed from tokenMove to avoid conflict).
type tokenMoveOut struct {
	Symbol   string
	Contract string
	Amount   decimal.Decimal
	Decimals int
}

// submitTrade constructs and submits a trade record.
// The "spent" side is the Quote (sold), the "received" side is the Ticker/Asset (bought).
// Returns (trades, transfers, skipped, error).
func submitTrade(hash, tsStr, account, feeAmount string, spent, received tokenMoveOut) (int, int, int, error) {
	price := ""
	if received.Amount.IsPositive() {
		price = calcPrice(spent.Amount, received.Amount)
	}

	trade := tax.PluginTrade{
		TxID:          hash,
		Ts:            tsStr,
		Account:       account,
		Action:        "BUY",
		Ticker:        received.Symbol,
		Asset:         received.Symbol,
		AssetAddress:  received.Contract,
		Amount:        received.Amount.String(),
		AssetDecimals: int32(received.Decimals),
		Quote:         spent.Symbol,
		QuoteAddress:  spent.Contract,
		Value:         spent.Amount.String(),
		QuoteDecimals: int32(spent.Decimals),
		Price:         price,
		FeeAmount:     feeAmount,
		FeeCurrency:   "ETH",
		FeeDecimals:   int32(ethDecimals),
	}

	if err := tax.SubmitTrade(trade); err != nil {
		log.DebugWithData("Submit trade error (possibly duplicate)", map[string]any{
			"hash":  hash,
			"error": err.Error(),
		})
	}

	return 1, 0, 0, nil
}

// submitTransfer submits a single transfer record. Returns (trades, transfers, skipped, error).
func submitTransfer(_ string, transfer tax.PluginTransfer) (int, int, int, error) {
	if err := tax.SubmitTransfer(transfer); err != nil {
		log.DebugWithData("Submit transfer error (possibly duplicate)", map[string]any{
			"txID":  transfer.TxID,
			"error": err.Error(),
		})
	}

	return 0, 1, 0, nil
}

// submitTokenTransfers submits individual transfer records for each token in moves.
func submitTokenTransfers(hash, tsStr, account, source, dest string, moves []tokenMoveOut, action, feeAmount string) (int, int, int, error) {
	for i, m := range moves {
		fee := ""
		feeC := ""
		var feeDec int32

		if i == 0 {
			fee = feeAmount
			feeC = "ETH"
			feeDec = int32(ethDecimals)
		}

		transfer := tax.PluginTransfer{
			TxID:          hash,
			Ts:            tsStr,
			Account:       account,
			Asset:         m.Symbol,
			AssetDecimals: int32(m.Decimals),
			AssetAddress:  m.Contract,
			Amount:        m.Amount.String(),
			Action:        action,
			Source:        source,
			Destination:   dest,
			Fee:           fee,
			FeeCurrency:   feeC,
			FeeDecimals:   feeDec,
		}

		if err := tax.SubmitTransfer(transfer); err != nil {
			log.DebugWithData("Submit token transfer error (possibly duplicate)", map[string]any{
				"txID":  hash,
				"error": err.Error(),
			})
		}
	}

	return 0, len(moves), 0, nil
}

// calcPrice computes the price as spent/received.
func calcPrice(spent, received decimal.Decimal) string {
	return spent.Div(received).String()
}
