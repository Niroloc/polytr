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
	"strconv"
	"strings"
	"sync"
	"time"
)

// chainlinkFeed reads the BTC/USD Chainlink aggregator on Polygon via JSON-RPC.
// This is the exact oracle Polymarket uses to settle BTC Up/Down markets
// (https://data.chain.link/streams/btc-usd).
//
// Multiple RPC endpoints are tried in round-robin order; on any error the feed
// rotates to the next endpoint so a single overloaded node doesn't stall pricing.
type chainlinkFeed struct {
	mu           sync.Mutex
	price        float64
	urls         []string // candidate RPC endpoints
	urlIdx       int      // index of the currently active endpoint
	client       *http.Client
	history      []priceRecord // recent readings for historical lookup
	refBlockNum  uint64        // latest known Polygon block number
	refBlockTime time.Time     // wall-clock time when refBlockNum was recorded
}

type priceRecord struct {
	t  time.Time
	px float64
}

// maxHistory covers 6 minutes at 3-second polling cadence.
const maxHistory = 120

// PriceAt returns the Chainlink price at (or nearest to) time t.
//
// Fast path: scan the local history ring buffer (±30 s tolerance).
// Slow path: if the history miss, estimate the Polygon block at time t from our
// most recent block reference and call latestAnswer() at that block. This covers
// the case where the collector started after the window opened.
func (f *chainlinkFeed) PriceAt(t time.Time) float64 {
	f.mu.Lock()
	var best float64
	bestDiff := time.Duration(1<<63 - 1)
	for _, r := range f.history {
		d := r.t.Sub(t)
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			bestDiff = d
			best = r.px
		}
	}
	refBlock := f.refBlockNum
	refTime := f.refBlockTime
	url := f.urls[f.urlIdx]
	f.mu.Unlock()

	if bestDiff <= 30*time.Second {
		return best
	}

	// Fall back to a historical eth_call at the estimated block.
	if refBlock == 0 || refTime.IsZero() {
		return 0
	}
	// Polygon averages ~2.3 s per block.
	const polygonBlockSec = 2.3
	diffSec := refTime.Sub(t).Seconds()
	blocksBack := int64(diffSec / polygonBlockSec)
	targetBlock := int64(refBlock) - blocksBack
	if targetBlock <= 0 {
		return 0
	}
	blockHex := fmt.Sprintf("0x%x", targetBlock)
	p, err := f.latestAnswerAt(url, blockHex)
	if err != nil {
		log.Printf("[btcprice] chainlink historical block %s: %v", blockHex, err)
		return 0
	}
	log.Printf("[btcprice] chainlink historical K=%.2f at block %s (Δblocks=%d)", p, blockHex, blocksBack)
	return p
}

const (
	// Chainlink BTC/USD aggregator on Polygon mainnet (8 decimals).
	chainlinkBTCUSD = "0xc907E116054Ad103354f2D350FD2514433D57F6f"
	// latestAnswer() ABI selector.
	latestAnswerSel = "0x50d25bcd"

	// DefaultPolygonRPC is the primary endpoint used when --polygon-rpc is omitted.
	DefaultPolygonRPC = "https://polygon.llamarpc.com"
)

// builtinRPCs is the fallback pool — free, no-key public Polygon endpoints.
var builtinRPCs = []string{
	"https://polygon.llamarpc.com",
	"https://polygon.drpc.org",
	"https://polygon-bor-rpc.publicnode.com",
	"https://rpc.ankr.com/polygon",
}

func NewChainlinkFeed(rpcURL string) Source {
	urls := make([]string, 0, len(builtinRPCs)+1)
	if rpcURL != "" {
		urls = append(urls, rpcURL)
	}
	for _, u := range builtinRPCs {
		if u != rpcURL {
			urls = append(urls, u)
		}
	}
	return &chainlinkFeed{
		urls:   urls,
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
	f.mu.Lock()
	primary := f.urls[0]
	f.mu.Unlock()
	log.Printf("[btcprice] chainlink: primary=%s  fallbacks=%d  contract=%s",
		primary, len(f.urls)-1, chainlinkBTCUSD)
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
	f.mu.Lock()
	startIdx := f.urlIdx
	f.mu.Unlock()

	for i := range f.urls {
		idx := (startIdx + i) % len(f.urls)
		p, err := f.latestAnswer(f.urls[idx])
		if err != nil {
			log.Printf("[btcprice] chainlink [%s]: %v — trying next", f.urls[idx], err)
			f.mu.Lock()
			f.urlIdx = (idx + 1) % len(f.urls)
			f.mu.Unlock()
			continue
		}
		now := time.Now()
		blockNum, _ := f.getBlockNumber(f.urls[idx])
		f.mu.Lock()
		f.price = p
		f.urlIdx = idx // stick with the working endpoint
		f.history = append(f.history, priceRecord{t: now, px: p})
		if len(f.history) > maxHistory {
			f.history = f.history[1:]
		}
		if blockNum > 0 {
			f.refBlockNum = blockNum
			f.refBlockTime = now
		}
		f.mu.Unlock()
		return
	}
}

// getBlockNumber returns the current block number from the RPC.
func (f *chainlinkFeed) getBlockNumber(rpcURL string) (uint64, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_blockNumber",
		"params":  []any{},
		"id":      2,
	})
	resp, err := f.client.Post(rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var rpcResp struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return 0, err
	}
	raw := strings.TrimPrefix(rpcResp.Result, "0x")
	n, err := strconv.ParseUint(raw, 16, 64)
	return n, err
}

// latestAnswerAt calls latestAnswer() at a specific block (hex string, e.g. "0x1a2b3c").
func (f *chainlinkFeed) latestAnswerAt(rpcURL, blockHex string) (float64, error) {
	return f.callLatestAnswer(rpcURL, blockHex)
}

// latestAnswer calls latestAnswer() on the Chainlink aggregator via eth_call.
func (f *chainlinkFeed) latestAnswer(rpcURL string) (float64, error) {
	return f.callLatestAnswer(rpcURL, "latest")
}

func (f *chainlinkFeed) callLatestAnswer(rpcURL, block string) (float64, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_call",
		"params": []any{
			map[string]string{
				"to":   chainlinkBTCUSD,
				"data": latestAnswerSel,
			},
			block,
		},
		"id": 1,
	})

	resp, err := f.client.Post(rpcURL, "application/json", bytes.NewReader(body))
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
	price := float64(n.Int64()) / 1e8
	return price, nil
}
