package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const wsMarketURL = "wss://ws-subscriptions-clob.polymarket.com/ws/market"

// MarketEvent is emitted on the Events() channel for every state-changing
// WebSocket update: top-of-book change (book/price_change) or a trade.
type MarketEvent struct {
	Type    string // "book" | "price_change" | "trade"
	TokenID string
	Ts      time.Time

	// Trade is populated only when Type=="trade".
	Trade *TradeTick
}

// TradeTick is a single trade reported via the last_trade_price WebSocket event.
// Note: the WS feed does not include transactionHash or taker wallet — these
// fields are only available via the REST data API.
type TradeTick struct {
	Price float64
	Size  float64
	Side  string // "BUY" or "SELL" (taker side)
}

// WSClient subscribes to Polymarket's market WebSocket and maintains an
// in-memory order book per subscribed asset. State-changing events are
// published on the Events() channel.
//
// Use:
//
//	c := NewWSClient()
//	go c.Run(ctx)
//	c.Subscribe([]string{tokenUp, tokenDown})
//	for ev := range c.Events() { ... }
type WSClient struct {
	events chan MarketEvent
	subCh  chan []string

	mu     sync.RWMutex
	books  map[string]*bookState // tokenID → state
	assets []string              // currently desired subscription list

	connMu sync.Mutex
	conn   *websocket.Conn
}

// bookState is the per-token order book held by WSClient. Levels are stored
// in price→size maps for O(1) updates; sorted slices are materialised on read.
type bookState struct {
	market string
	bids   map[float64]float64 // price → size
	asks   map[float64]float64 // price → size
}

func newBookState() *bookState {
	return &bookState{bids: make(map[float64]float64), asks: make(map[float64]float64)}
}

func (b *bookState) bestBid() float64 {
	best := 0.0
	for p, s := range b.bids {
		if s == 0 {
			continue
		}
		if p > best {
			best = p
		}
	}
	return best
}

func (b *bookState) bestAsk() float64 {
	best := 0.0
	for p, s := range b.asks {
		if s == 0 {
			continue
		}
		if best == 0 || p < best {
			best = p
		}
	}
	return best
}

func NewWSClient() *WSClient {
	return &WSClient{
		events: make(chan MarketEvent, 1024),
		subCh:  make(chan []string, 4),
		books:  make(map[string]*bookState),
	}
}

// Events is the channel of state-changing market events.
func (c *WSClient) Events() <-chan MarketEvent { return c.events }

// Subscribe replaces the current asset subscription list. Safe to call from
// any goroutine. The actual subscribe message is sent on the WS goroutine.
func (c *WSClient) Subscribe(assetIDs []string) {
	c.subCh <- assetIDs
}

// BookSnapshot returns a copy of the current order book for tokenID
// in the legacy OrderBook structure (sorted slices). Returns nil if no
// book has been received yet for this token.
func (c *WSClient) BookSnapshot(tokenID string) *OrderBook {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.books[tokenID]
	if !ok {
		return nil
	}
	if len(b.bids) == 0 && len(b.asks) == 0 {
		return nil
	}
	ob := &OrderBook{MarketID: b.market, TokenID: tokenID, Ts: time.Now()}
	type pair struct{ price, size float64 }
	bids := make([]pair, 0, len(b.bids))
	for p, s := range b.bids {
		if s > 0 {
			bids = append(bids, pair{p, s})
		}
	}
	sort.Slice(bids, func(i, j int) bool { return bids[i].price < bids[j].price }) // ascending: best at end
	for _, x := range bids {
		ob.Bids = append(ob.Bids, OrderBookLevel{Price: x.price, Size: x.size})
	}
	asks := make([]pair, 0, len(b.asks))
	for p, s := range b.asks {
		if s > 0 {
			asks = append(asks, pair{p, s})
		}
	}
	sort.Slice(asks, func(i, j int) bool { return asks[i].price > asks[j].price }) // descending: best at end
	for _, x := range asks {
		ob.Asks = append(ob.Asks, OrderBookLevel{Price: x.price, Size: x.size})
	}
	return ob
}

// Run connects to the WebSocket and reads until ctx is cancelled.
// Reconnects with 2-second backoff on any error.
func (c *WSClient) Run(ctx context.Context) {
	for {
		if err := c.runOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[ws] reconnect after error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *WSClient) runOnce(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsMarketURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		c.conn = nil
		c.connMu.Unlock()
	}()

	log.Printf("[ws] connected to %s", wsMarketURL)

	// On reconnect: if we already have a subscription list, resubscribe now.
	c.mu.RLock()
	assets := append([]string(nil), c.assets...)
	c.mu.RUnlock()
	if len(assets) > 0 {
		if err := c.sendSubscribe(assets); err != nil {
			return fmt.Errorf("resubscribe: %w", err)
		}
		log.Printf("[ws] resubscribed to %d assets", len(assets))
	}

	readErr := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			c.processMessage(raw)
		}
	}()

	// Keepalive: Polymarket closes idle connections after ~30s. Send a PING
	// text frame every 10s to keep the connection alive.
	keepalive := time.NewTicker(10 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-keepalive.C:
			c.connMu.Lock()
			err := conn.WriteMessage(websocket.TextMessage, []byte("PING"))
			c.connMu.Unlock()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		case ids := <-c.subCh:
			c.mu.Lock()
			// Drop in-memory state for tokens we are no longer following.
			newSet := make(map[string]bool, len(ids))
			for _, id := range ids {
				newSet[id] = true
			}
			for tid := range c.books {
				if !newSet[tid] {
					delete(c.books, tid)
				}
			}
			c.assets = ids
			c.mu.Unlock()
			if err := c.sendSubscribe(ids); err != nil {
				return fmt.Errorf("subscribe: %w", err)
			}
			log.Printf("[ws] subscribed to %d assets", len(ids))
		}
	}
}

func (c *WSClient) sendSubscribe(assetIDs []string) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	msg := struct {
		Type     string   `json:"type"`
		AssetIDs []string `json:"assets_ids"`
	}{"Market", assetIDs}
	return c.conn.WriteJSON(msg)
}

func (c *WSClient) processMessage(raw []byte) {
	// Polymarket batches multiple events into a JSON array sometimes.
	if len(raw) > 0 && raw[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			for _, m := range arr {
				c.processSingle(m)
			}
			return
		}
	}
	c.processSingle(raw)
}

func (c *WSClient) processSingle(raw []byte) {
	var peek struct {
		EventType string `json:"event_type"`
		AssetID   string `json:"asset_id"`
		Market    string `json:"market"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return
	}
	switch peek.EventType {
	case "book":
		c.handleBook(raw, peek.AssetID, peek.Market)
	case "price_change":
		c.handlePriceChange(raw, peek.AssetID)
	case "last_trade_price":
		c.handleTrade(raw, peek.AssetID)
	}
}

func (c *WSClient) handleBook(raw []byte, assetID, market string) {
	var m struct {
		Bids []apiBookLevel `json:"bids"`
		Asks []apiBookLevel `json:"asks"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	state := newBookState()
	state.market = market
	for _, b := range m.Bids {
		p, _ := strconv.ParseFloat(b.Price, 64)
		s, _ := strconv.ParseFloat(b.Size, 64)
		if s > 0 {
			state.bids[p] = s
		}
	}
	for _, a := range m.Asks {
		p, _ := strconv.ParseFloat(a.Price, 64)
		s, _ := strconv.ParseFloat(a.Size, 64)
		if s > 0 {
			state.asks[p] = s
		}
	}

	c.mu.Lock()
	prev := c.books[assetID]
	prevBid, prevAsk := 0.0, 0.0
	if prev != nil {
		prevBid = prev.bestBid()
		prevAsk = prev.bestAsk()
	}
	c.books[assetID] = state
	newBid := state.bestBid()
	newAsk := state.bestAsk()
	c.mu.Unlock()

	// Always emit on a fresh book event (first one establishes initial state).
	// Subsequent book events suppress emission unless top-of-book changed.
	if prev == nil || newBid != prevBid || newAsk != prevAsk {
		c.emit(MarketEvent{Type: "book", TokenID: assetID, Ts: time.Now()})
	}
}

func (c *WSClient) handlePriceChange(raw []byte, assetID string) {
	var m struct {
		Changes []struct {
			Price string `json:"price"`
			Side  string `json:"side"`
			Size  string `json:"size"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}

	c.mu.Lock()
	state, ok := c.books[assetID]
	if !ok {
		c.mu.Unlock()
		return // price_change before initial book — ignore
	}
	prevBid := state.bestBid()
	prevAsk := state.bestAsk()
	for _, ch := range m.Changes {
		p, _ := strconv.ParseFloat(ch.Price, 64)
		s, _ := strconv.ParseFloat(ch.Size, 64)
		switch ch.Side {
		case "BUY":
			if s == 0 {
				delete(state.bids, p)
			} else {
				state.bids[p] = s
			}
		case "SELL":
			if s == 0 {
				delete(state.asks, p)
			} else {
				state.asks[p] = s
			}
		}
	}
	newBid := state.bestBid()
	newAsk := state.bestAsk()
	c.mu.Unlock()

	if newBid != prevBid || newAsk != prevAsk {
		c.emit(MarketEvent{Type: "price_change", TokenID: assetID, Ts: time.Now()})
	}
}

func (c *WSClient) handleTrade(raw []byte, assetID string) {
	var m struct {
		Price string `json:"price"`
		Size  string `json:"size"`
		Side  string `json:"side"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	price, _ := strconv.ParseFloat(m.Price, 64)
	size, _ := strconv.ParseFloat(m.Size, 64)
	c.emit(MarketEvent{
		Type:    "trade",
		TokenID: assetID,
		Ts:      time.Now(),
		Trade:   &TradeTick{Price: price, Size: size, Side: m.Side},
	})
}

func (c *WSClient) emit(ev MarketEvent) {
	select {
	case c.events <- ev:
	default:
		log.Printf("[ws] event channel full, dropped %s for %.8s", ev.Type, ev.TokenID)
	}
}
