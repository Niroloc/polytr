package trader

import (
	"log"
	"time"
)

// NearExpiryStrategy enters only in the final 30–120 seconds of a window,
// when binary options have maximal gamma (price sensitivity to spot moves).
// It uses the same edge signal as Strategy but restricts the entry window.
type NearExpiryStrategy struct {
	params Params
}

func NewNearExpiryStrategy(p Params) *NearExpiryStrategy {
	if p.Name == "" {
		p.Name = "near_expiry"
	}
	return &NearExpiryStrategy{params: p}
}

func (s *NearExpiryStrategy) Name() string { return s.params.Name }

func (s *NearExpiryStrategy) Evaluate(snap Snapshot) *TradeSignal {
	tte := time.Until(snap.Expiry)
	if tte < 30*time.Second || tte > 120*time.Second {
		return nil // only trade in the final 2 minutes
	}
	if snap.Spread > 3*s.params.EdgeThreshold {
		return nil
	}
	if snap.MidPrice <= 0 || snap.FairPrice <= 0 {
		return nil
	}
	if snap.Edge < s.params.EdgeThreshold {
		return nil // BUY-only, same as paper edge strategy
	}

	size := s.params.MaxSizeUSDC
	if size < s.params.MinSizeUSDC {
		return nil
	}

	price := snap.FairPrice
	if price < 0.01 {
		price = 0.01
	}
	if price > 0.99 {
		price = 0.99
	}

	log.Printf("[strategy/%s] signal BUY %s @ %.4f edge=%+.4f tte=%.0fs size=%.2f USDC",
		s.params.Name, snap.TokenID[:min(8, len(snap.TokenID))], price, snap.Edge, tte.Seconds(), size)

	return &TradeSignal{
		TokenID:  snap.TokenID,
		MarketID: snap.MarketID,
		Outcome:  snap.Outcome,
		Side:     Buy,
		Price:    price,
		SizeUSDC: size,
		Expiry:   snap.Expiry,
		Edge:     snap.Edge,
	}
}
