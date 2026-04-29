package btcprice

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// chainlinkFeed reads the BTC/USD Chainlink aggregator on Polygon via JSON-RPC.
// This is the exact oracle Polymarket uses to settle BTC Up/Down markets
// (https://data.chain.link/streams/btc-usd).
type chainlinkFeed struct {
	mu     sync.Mutex
	price  float64
	rpcURL string
	client *http.Client
}

const (
	// Chainlink BTC/USD aggregator on Polygon mainnet (8 decimals).
	chainlinkBTCUSD = "0xc907E116054Ad103354f2D350FD2514433D57F6f"
	// latestAnswer() ABI selector.
	latestAnswerSel = "0x50d25bcd"

	DefaultPolygonRPC = "https://1rpc.io/matic"
)

func NewChainlinkFeed(rpcURL string) Source {
	if rpcURL == "" {
		rpcURL = DefaultPolygonRPC
	}
	return &chainlinkFeed{
		rpcURL: rpcURL,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (f *chainlinkFeed) Price() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.price
}

func (f *chainlinkFeed) WaitPrice(ctx context.Context) float64 {
	for {
		if p := f.Price(); p > 0 {
			return p
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (f *chainlinkFeed) Run(ctx context.Context) {
	log.Printf("[btcprice] chainlink: polling %s (contract %s)", f.rpcURL, chainlinkBTCUSD)
	f.fetch()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.fetch()
		}
	}
}

func (f *chainlinkFeed) fetch() {
	p, err := f.latestAnswer()
	if err != nil {
		log.Printf("[btcprice] chainlink: %v", err)
		return
	}
	f.mu.Lock()
	f.price = p
	f.mu.Unlock()
}

// latestAnswer calls latestAnswer() on the Chainlink aggregator via eth_call.
func (f *chainlinkFeed) latestAnswer() (float64, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_call",
		"params": []any{
			map[string]string{
				"to":   chainlinkBTCUSD,
				"data": latestAnswerSel,
			},
			"latest",
		},
		"id": 1,
	})

	resp, err := f.client.Post(f.rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("rpc: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	if rpcResp.Error != nil {
		return 0, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	raw := strings.TrimPrefix(rpcResp.Result, "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return 0, fmt.Errorf("hex %q: %w", raw, err)
	}
	// int256 big-endian; BTC/USD is always positive so treat as unsigned.
	n := new(big.Int).SetBytes(b)
	// Chainlink BTC/USD has 8 decimal places.
	price := float64(n.Int64()) / 1e8
	return price, nil
}
