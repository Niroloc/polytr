package trader

import (
	"log"
	"time"
)

// StraddleStrategy accumulates positions in both Up and Down legs independently,
// aiming to hold both sides before window expiry. Each leg is evaluated on every
// tick: if its bestAsk < 0.50 and Black-Scholes fair price exceeds bestAsk by at
// least EdgeThreshold, a BUY signal is emitted at the ask price.
//
// The corresponding PaperTrader must be configured with NoNet:true and
// HoldToExpiry:true so that opposite-direction positions are not netted and
// both legs are held until settlement.
type StraddleStrategy struct {
	params Params
}

func NewStraddleStrategy(p Params) *StraddleStrategy {
	return &StraddleStrategy{params: p}
}

func (s *StraddleStrategy) Name() string { return s.params.Name }

// Evaluate satisfies StrategySignaler; always returns nil because EvaluateBatch
// is the active entry point for this strategy.
func (s *StraddleStrategy) Evaluate(_ Snapshot) *TradeSignal { return nil }

// EvaluateBatch implements BatchSignaler. Each snapshot is evaluated
// independently — no requirement for both legs to be underpriced simultaneously.
func (s *StraddleStrategy) EvaluateBatch(snaps []Snapshot) []*TradeSignal {
	var signals []*TradeSignal

	for _, snap := range snaps {
		if snap.Expiry.IsZero() || time.Until(snap.Expiry) < 30*time.Second {
			continue
		}
		if snap.BestAsk <= 0 || snap.BestAsk >= 0.50 {
			continue // only enter below 50 cents
		}
		if snap.FairPrice <= snap.BestAsk {
			continue // not underpriced at the ask
		}
		edge := snap.FairPrice - snap.BestAsk
		if edge < s.params.EdgeThreshold {
			continue
		}
		if snap.Spread > 3*s.params.EdgeThreshold {
			continue // spread too wide
		}

		size := s.params.MaxSizeUSDC
		if size < s.params.MinSizeUSDC {
			continue
		}

		log.Printf("[strategy/%s] %s ask=%.4f fair=%.4f edge=%+.4f size=%.2f USDC",
			s.params.Name, snap.Outcome, snap.BestAsk, snap.FairPrice, edge, size)

		signals = append(signals, &TradeSignal{
			TokenID:  snap.TokenID,
			MarketID: snap.MarketID,
			Outcome:  snap.Outcome,
			Side:     Buy,
			Price:    snap.BestAsk, // fill at ask
			SizeUSDC: size,
			Expiry:   time.Now().Add(s.params.OrderTTL),
			Edge:     edge,
		})
	}

	return signals
}
