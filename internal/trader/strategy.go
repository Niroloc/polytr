package trader

import (
	"log"
	"sync"
	"time"
)

// Snapshot is the market state passed to the strategy each poll cycle.
type Snapshot struct {
	TokenID  string
	MarketID string
	Outcome  string
	MidPrice float64
	FairPrice float64
	Edge     float64 // fair - mid; positive = underpriced YES
	Spread   float64
	Expiry   time.Time
}

// Params controls the simple edge strategy.
type Params struct {
	EdgeThreshold float64
	MaxSizeUSDC   float64
	MinSizeUSDC   float64
	PriceOffset   float64 // how far from mid to place (e.g. 0.01)
	OrderTTL      time.Duration
}

// Strategy decides whether to trade based on edge.
// It tracks one open order per token to avoid stacking positions.
type Strategy struct {
	params     Params
	mu         sync.Mutex
	openOrders map[string]string // tokenID → orderID
}

func NewStrategy(p Params) *Strategy {
	return &Strategy{
		params:     p,
		openOrders: make(map[string]string),
	}
}

// Evaluate returns a TradeSignal if conditions are met, or nil to do nothing.
//
// Logic:
//   - Skip if spread is too wide (> 3× edge threshold — likely illiquid)
//   - Skip if we already have an open order for this token
//   - Skip if TTE < 30s (too close to expiry)
//   - BUY YES if edge > threshold (market underprices YES)
//   - SELL YES (= BUY NO) if edge < -threshold (market overprices YES)
//
// Price is set at mid ± PriceOffset to rest as a passive limit order.
func (s *Strategy) Evaluate(snap Snapshot) *TradeSignal {
	if snap.Expiry.IsZero() || time.Until(snap.Expiry) < 30*time.Second {
		return nil
	}
	if snap.Spread > 3*s.params.EdgeThreshold {
		return nil
	}
	if snap.MidPrice <= 0 {
		return nil
	}

	s.mu.Lock()
	_, hasOpen := s.openOrders[snap.TokenID]
	s.mu.Unlock()
	if hasOpen {
		return nil
	}

	absEdge := snap.Edge
	if absEdge < 0 {
		absEdge = -absEdge
	}
	if absEdge < s.params.EdgeThreshold {
		return nil
	}

	var side Side
	var price float64
	if snap.Edge > 0 {
		// YES is underpriced → BUY at mid - offset (passive)
		side = Buy
		price = snap.MidPrice - s.params.PriceOffset
	} else {
		// YES is overpriced → SELL at mid + offset (passive)
		side = Sell
		price = snap.MidPrice + s.params.PriceOffset
	}

	// Clamp price to [0.01, 0.99]
	if price < 0.01 {
		price = 0.01
	}
	if price > 0.99 {
		price = 0.99
	}

	size := s.params.MaxSizeUSDC
	if size < s.params.MinSizeUSDC {
		return nil
	}

	log.Printf("[strategy] signal %s %s @ %.4f edge=%+.4f size=%.2f USDC",
		side, snap.TokenID[:min(8, len(snap.TokenID))], price, snap.Edge, size)

	return &TradeSignal{
		TokenID:  snap.TokenID,
		MarketID: snap.MarketID,
		Outcome:  snap.Outcome,
		Side:     side,
		Price:    price,
		SizeUSDC: size,
		Expiry:   time.Now().Add(s.params.OrderTTL),
		Edge:     snap.Edge,
	}
}

// RecordOpen notes that an order was placed for a token.
func (s *Strategy) RecordOpen(tokenID, orderID string) {
	s.mu.Lock()
	s.openOrders[tokenID] = orderID
	s.mu.Unlock()
}

// RecordClosed removes a token from the open-orders map.
func (s *Strategy) RecordClosed(tokenID string) {
	s.mu.Lock()
	delete(s.openOrders, tokenID)
	s.mu.Unlock()
}

// OpenOrderID returns the current open order ID for a token, or "".
func (s *Strategy) OpenOrderID(tokenID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openOrders[tokenID]
}
