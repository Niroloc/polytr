package trader

import (
	"log"
	"sync"
	"time"

	"trading-polymarket/internal/csvlog"
	"trading-polymarket/internal/metrics"
)

// PaperTrader simulates strategy execution without real orders.
// It is used in collect-only mode to track hypothetical P&L.
//
// Risk budget: total USDC spent across all trades in a 5-minute window is
// capped at Params.MaxWindowRiskUSDC. Individual trade size is reduced to fit
// the remaining budget; if the remainder is below MinSizeUSDC the trade is skipped.
//
// P&L model (mark-to-market, closed at final mid on window expiry):
//
//	BUY  N contracts at entry E, close at M: P&L = N × (M − E)
//	SELL N contracts at entry E, close at M: P&L = N × (E − M)
//	where N = SizeUSDC / E
type PaperTrader struct {
	strategy   *Strategy
	windowLog  *csvlog.WindowWriter // may be nil

	mu          sync.Mutex
	pos         map[string]*paperPos
	windowCost  float64
	dailyPnL    float64
	totalPnL    float64
	currentDay  string // UTC date YYYY-MM-DD of the last closed window
}

type paperPos struct {
	outcome    string
	side       Side
	entryPrice float64
	shares     float64
	cost       float64
}

func NewPaperTrader(p Params, wl *csvlog.WindowWriter) *PaperTrader {
	pt := &PaperTrader{
		strategy:   NewStrategy(p),
		windowLog:  wl,
		pos:        make(map[string]*paperPos),
		currentDay: time.Now().UTC().Format("2006-01-02"),
	}
	metrics.PaperWindowRiskLimit.Set(p.MaxWindowRiskUSDC)
	return pt
}

// OnTick updates unrealized P&L and position metrics, then evaluates the
// strategy and paper-executes a signal if the risk budget allows.
func (pt *PaperTrader) OnTick(snap Snapshot) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if p, ok := pt.pos[snap.TokenID]; ok {
		metrics.PaperUnrealizedPnL.WithLabelValues(p.outcome).Set(pt.pnl(p, snap.MidPrice))
		metrics.PaperPositionContracts.WithLabelValues(p.outcome).Set(pt.signedContracts(p))
	}

	sig := pt.strategy.Evaluate(snap)
	if sig == nil {
		return
	}

	entry := snap.MidPrice
	if entry <= 0 {
		return
	}

	size := sig.SizeUSDC
	maxRisk := pt.strategy.params.MaxWindowRiskUSDC
	if maxRisk > 0 {
		remaining := maxRisk - pt.windowCost
		if remaining <= 0 {
			log.Printf("[paper] window risk budget exhausted (%.2f/%.2f USDC)", pt.windowCost, maxRisk)
			return
		}
		if size > remaining {
			size = remaining
		}
		if size < pt.strategy.params.MinSizeUSDC {
			log.Printf("[paper] insufficient budget for min size (%.2f remaining)", remaining)
			return
		}
	}

	pos := &paperPos{
		outcome:    snap.Outcome,
		side:       sig.Side,
		entryPrice: entry,
		shares:     size / entry,
		cost:       size,
	}
	pt.pos[snap.TokenID] = pos
	pt.windowCost += size
	pt.strategy.RecordOpen(snap.TokenID, "paper")

	log.Printf("[paper] %s %s %.4f @ %.4f  cost=%.2f USDC  window=%.2f/%.2f",
		sig.Side, snap.Outcome, pos.shares, entry, size, pt.windowCost, maxRisk)

	metrics.PaperTradeEntryPrice.WithLabelValues(snap.Outcome, sig.Side.String()).Set(entry)
	metrics.PaperTradeSize.WithLabelValues(snap.Outcome, sig.Side.String()).Set(size)
	metrics.PaperTradesTotal.WithLabelValues(snap.Outcome, sig.Side.String()).Inc()
	metrics.PaperPositionContracts.WithLabelValues(snap.Outcome).Set(pt.signedContracts(pos))
	metrics.PaperWindowRiskUsed.Set(pt.windowCost)
}

// OnExpiry closes any open position for tokenID at finalMid, books P&L, and
// returns a WindowRecord suitable for CSV logging. Always returns a record
// (with zeros if no position was held) so every window is accounted for.
func (pt *PaperTrader) OnExpiry(tokenID, outcome string, finalMid float64) csvlog.WindowRecord {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	if today != pt.currentDay {
		pt.dailyPnL = 0
		pt.currentDay = today
		metrics.PaperDailyPnL.Set(0)
	}

	rec := csvlog.WindowRecord{
		ClosedAt: now,
		Outcome:  outcome,
	}

	p, ok := pt.pos[tokenID]
	if !ok {
		// No position held this window — write zero row.
		rec.DailyPnL = pt.dailyPnL
		rec.TotalPnL = pt.totalPnL
		metrics.PaperUnrealizedPnL.WithLabelValues(outcome).Set(0)
		metrics.PaperPositionContracts.WithLabelValues(outcome).Set(0)
		pt.writeWindow(rec)
		return rec
	}

	realized := pt.pnl(p, finalMid)
	pt.dailyPnL += realized
	pt.totalPnL += realized
	delete(pt.pos, tokenID)

	rec.Side = p.side.String()
	rec.EntryPrice = p.entryPrice
	rec.ExitPrice = finalMid
	rec.Contracts = pt.signedContracts(p)
	rec.CostUSDC = p.cost
	rec.RealizedPnL = realized
	rec.DailyPnL = pt.dailyPnL
	rec.TotalPnL = pt.totalPnL

	log.Printf("[paper] closed %s @ %.4f  realized=%+.4f  daily=%+.4f  total=%+.4f",
		outcome, finalMid, realized, pt.dailyPnL, pt.totalPnL)

	metrics.PaperWindowPnL.WithLabelValues(outcome).Set(realized)
	metrics.PaperUnrealizedPnL.WithLabelValues(outcome).Set(0)
	metrics.PaperPositionContracts.WithLabelValues(outcome).Set(0)
	metrics.PaperTotalPnL.Set(pt.totalPnL)
	metrics.PaperDailyPnL.Set(pt.dailyPnL)

	pt.writeWindow(rec)
	return rec
}

// OnNewWindow resets the per-window risk budget. Call when a new 5-minute
// window is discovered and the token list is replaced.
func (pt *PaperTrader) OnNewWindow() {
	pt.mu.Lock()
	pt.windowCost = 0
	pt.mu.Unlock()
	metrics.PaperWindowRiskUsed.Set(0)
}

func (pt *PaperTrader) writeWindow(r csvlog.WindowRecord) {
	if pt.windowLog == nil {
		return
	}
	if err := pt.windowLog.Write(r); err != nil {
		log.Printf("[paper] window log: %v", err)
	}
}

func (pt *PaperTrader) pnl(p *paperPos, currentMid float64) float64 {
	if p.side == Buy {
		return p.shares * (currentMid - p.entryPrice)
	}
	return p.shares * (p.entryPrice - currentMid)
}

func (pt *PaperTrader) signedContracts(p *paperPos) float64 {
	if p.side == Buy {
		return p.shares
	}
	return -p.shares
}
