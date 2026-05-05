package trader

import (
	"log"
	"sync"
	"time"

	"trading-polymarket/internal/csvlog"
	"trading-polymarket/internal/metrics"
)

// PaperExecutorParams controls position management and risk limits for a
// PaperTrader. These are execution-layer concerns, independent of strategy logic.
type PaperExecutorParams struct {
	MaxWindowRiskUSDC float64 // max |net| USDC per 5-minute window (0 = unlimited)
	MinSizeUSDC       float64 // minimum order size; signals below this are skipped
	LossCutEdge       float64 // close at loss when edge < -LossCutEdge (0 = never cut)
	HoldToExpiry      bool    // if true, never close positions mid-window; settle at expiry only
	NoNet             bool    // if true, do not close opposite-direction positions on entry (straddle)
}

// PaperTrader simulates strategy execution without real orders.
//
// Position model:
//
//	netUSDC = Σ cost(Up positions) − Σ cost(Down positions)
//	+N means long Up by $N, −N means long Down by $N
//	|netUSDC| ≤ MaxWindowRiskUSDC
//
// A signal fires on every tick where the strategy returns a BUY signal.
// If a position in the same direction is already open, the new size is added
// (weighted-average entry price) up to the budget limit. If a position in the
// opposite direction is open, it is closed first at the last known bid (netting),
// then the new position is opened.
//
// Settlement: at window end every remaining position is closed at 1.0 (ITM)
// or 0.0 (OTM).  P&L = shares × (settlement − entryPrice).
type PaperTrader struct {
	sig       StrategySignaler
	ep        PaperExecutorParams
	windowLog *csvlog.WindowWriter // may be nil

	mu         sync.Mutex
	pos        map[string]*paperPos // tokenID → open position
	warmup     bool                 // true during the first window — no orders placed
	dailyPnL   float64
	totalPnL   float64
	currentDay string // UTC date YYYY-MM-DD
}

type paperPos struct {
	outcome    string
	entryPrice float64 // weighted-average entry across all adds
	shares     float64 // total contracts held
	cost       float64 // total USDC spent
	lastBid    float64 // most recent bid price; used for mark-to-market and mid-window close
}

func NewPaperTrader(sig StrategySignaler, ep PaperExecutorParams, wl *csvlog.WindowWriter) *PaperTrader {
	pt := &PaperTrader{
		sig:        sig,
		ep:         ep,
		windowLog:  wl,
		pos:        make(map[string]*paperPos),
		warmup:     true,
		currentDay: time.Now().UTC().Format("2006-01-02"),
	}
	metrics.PaperWindowRiskLimit.WithLabelValues(sig.Name()).Set(ep.MaxWindowRiskUSDC)
	return pt
}

// netUSDC returns the current net position in USDC:
//
//	positive = long Up, negative = long Down
func (pt *PaperTrader) netUSDC() float64 {
	var net float64
	for _, p := range pt.pos {
		if p.outcome == "Up" {
			net += p.cost
		} else {
			net -= p.cost
		}
	}
	return net
}

func oppositeOutcome(o string) string {
	if o == "Up" {
		return "Down"
	}
	return "Up"
}

// OnTick updates unrealized P&L, then evaluates the strategy. A BUY signal
// fires on every tick where the strategy says so:
//   - opposite position open → close it first (netting), then enter
//   - same-direction position open → add to it (weighted avg entry) up to limit
//   - no position → open fresh
func (pt *PaperTrader) OnTick(snap Snapshot) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	name := pt.sig.Name()
	pt.rollDayIfNeeded()

	// Update lastBid and unrealized P&L (marked at bid — the price we can sell at).
	if p, ok := pt.pos[snap.TokenID]; ok {
		p.lastBid = snap.BestBid
		unrealized := pt.pnl(p, snap.BestBid)
		metrics.PaperUnrealizedPnL.WithLabelValues(name, p.outcome).Set(unrealized)

		// Close threshold: take profit as soon as edge is gone (≤0);
		// cut a loss only when edge exceeds LossCutEdge against us.
		closeEdge := 0.0
		if unrealized < 0 && pt.ep.LossCutEdge > 0 {
			closeEdge = -pt.ep.LossCutEdge
		}
		if snap.Edge <= closeEdge {
			pt.bookPnL(unrealized)
			delete(pt.pos, snap.TokenID)

			log.Printf("[paper/%s] close %s @ %.4f bid (edge=%.4f thr=%.4f)  realized=%+.4f  daily=%+.4f",
				name, p.outcome, snap.BestBid, snap.Edge, closeEdge, unrealized, pt.dailyPnL)

			metrics.PaperWindowPnL.WithLabelValues(name, p.outcome).Set(unrealized)
			metrics.PaperUnrealizedPnL.WithLabelValues(name, p.outcome).Set(0)
			metrics.PaperPositionNetUSDC.WithLabelValues(name).Set(pt.netUSDC())
			metrics.PaperTotalPnL.WithLabelValues(name).Set(pt.totalPnL)
			metrics.PaperDailyPnL.WithLabelValues(name).Set(pt.dailyPnL)
			return
		}
	}

	if pt.warmup {
		return // first window is observation-only
	}

	sig := pt.sig.Evaluate(snap)
	if sig == nil || sig.Side != Buy {
		return
	}

	// Fill at sig.Price when set (e.g. straddle uses bestAsk); fall back to fair price.
	entry := sig.Price
	if entry <= 0 {
		entry = snap.FairPrice
	}
	if entry <= 0 {
		return
	}

	// Close any open position in the opposite direction before entering (netting).
	// Skipped for strategies that intentionally hold both sides (e.g. straddle).
	if !pt.ep.NoNet {
		opp := oppositeOutcome(snap.Outcome)
		for tid, p := range pt.pos {
			if p.outcome != opp {
				continue
			}
			closeAt := p.lastBid
			if closeAt <= 0 {
				closeAt = p.entryPrice
			}
			realized := pt.pnl(p, closeAt)
			pt.bookPnL(realized)
			delete(pt.pos, tid)

			log.Printf("[paper/%s] net flip: closed %s @ %.4f bid  realized=%+.4f  → opening %s",
				name, p.outcome, closeAt, realized, snap.Outcome)

			metrics.PaperWindowPnL.WithLabelValues(name, p.outcome).Set(realized)
			metrics.PaperUnrealizedPnL.WithLabelValues(name, p.outcome).Set(0)
			metrics.PaperTotalPnL.WithLabelValues(name).Set(pt.totalPnL)
			metrics.PaperDailyPnL.WithLabelValues(name).Set(pt.dailyPnL)
		}
	}

	// How much budget is left in this direction?
	maxRisk := pt.ep.MaxWindowRiskUSDC
	currentNet := pt.netUSDC()
	size := sig.SizeUSDC

	if maxRisk > 0 {
		var room float64
		if snap.Outcome == "Up" {
			room = maxRisk - currentNet
		} else {
			room = maxRisk + currentNet
		}
		if room < pt.ep.MinSizeUSDC {
			return // budget exhausted
		}
		if size > room {
			size = room
		}
	}

	newShares := size / entry

	if existing, ok := pt.pos[snap.TokenID]; ok {
		// Add to existing same-direction position; weighted-average entry price.
		totalShares := existing.shares + newShares
		existing.entryPrice = (existing.shares*existing.entryPrice + newShares*entry) / totalShares
		existing.shares = totalShares
		existing.cost += size
		existing.lastBid = snap.BestBid

		log.Printf("[paper/%s] ADD %s %.4f @ %.4f fair  +%.2f USDC  total_cost=%.2f  net=%.2f",
			name, snap.Outcome, newShares, entry, size, existing.cost, pt.netUSDC())
	} else {
		pt.pos[snap.TokenID] = &paperPos{
			outcome:    snap.Outcome,
			entryPrice: entry,
			shares:     newShares,
			cost:       size,
			lastBid:    snap.BestBid,
		}
		log.Printf("[paper/%s] BUY %s %.4f @ %.4f fair  cost=%.2f USDC  net=%.2f",
			name, snap.Outcome, newShares, entry, size, pt.netUSDC())
	}

	metrics.PaperTradeEntryPrice.WithLabelValues(name, snap.Outcome, "BUY").Set(pt.pos[snap.TokenID].entryPrice)
	metrics.PaperTradeSize.WithLabelValues(name, snap.Outcome, "BUY").Set(size)
	metrics.PaperTradesTotal.WithLabelValues(name, snap.Outcome, "BUY").Inc()
	metrics.PaperPositionNetUSDC.WithLabelValues(name).Set(pt.netUSDC())
}

// OnTickBatch is the unified entry point for each poll cycle.
//
// For strategies implementing BatchSignaler: passes all snapshots at once so the
// strategy can evaluate cross-token conditions (e.g. straddle).
// For regular StrategySignaler: falls back to calling Evaluate per snapshot.
//
// Position mark-to-market and close logic run first (across all open positions),
// then new signals are processed.
func (pt *PaperTrader) OnTickBatch(snaps []Snapshot) {
	if len(snaps) == 0 {
		return
	}

	byToken := make(map[string]Snapshot, len(snaps))
	for _, s := range snaps {
		byToken[s.TokenID] = s
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	name := pt.sig.Name()
	pt.rollDayIfNeeded()

	// 1. Mark-to-market and optional mid-window close for all open positions.
	for tokenID, p := range pt.pos {
		snap, ok := byToken[tokenID]
		if !ok {
			continue
		}
		p.lastBid = snap.BestBid
		unrealized := pt.pnl(p, snap.BestBid)
		metrics.PaperUnrealizedPnL.WithLabelValues(name, p.outcome).Set(unrealized)

		if !pt.ep.HoldToExpiry {
			closeEdge := 0.0
			if unrealized < 0 && pt.ep.LossCutEdge > 0 {
				closeEdge = -pt.ep.LossCutEdge
			}
			if snap.Edge <= closeEdge {
				pt.bookPnL(unrealized)
				delete(pt.pos, tokenID)
				log.Printf("[paper/%s] close %s @ %.4f bid (edge=%.4f thr=%.4f)  realized=%+.4f  daily=%+.4f",
					name, p.outcome, snap.BestBid, snap.Edge, closeEdge, unrealized, pt.dailyPnL)
				metrics.PaperWindowPnL.WithLabelValues(name, p.outcome).Set(unrealized)
				metrics.PaperUnrealizedPnL.WithLabelValues(name, p.outcome).Set(0)
				metrics.PaperPositionNetUSDC.WithLabelValues(name).Set(pt.netUSDC())
				metrics.PaperTotalPnL.WithLabelValues(name).Set(pt.totalPnL)
				metrics.PaperDailyPnL.WithLabelValues(name).Set(pt.dailyPnL)
			}
		}
	}

	if pt.warmup {
		return
	}

	// 2. Collect new entry signals.
	var sigs []*TradeSignal
	if bs, ok := pt.sig.(BatchSignaler); ok {
		sigs = bs.EvaluateBatch(snaps)
	} else {
		for _, snap := range snaps {
			if sig := pt.sig.Evaluate(snap); sig != nil {
				sigs = append(sigs, sig)
			}
		}
	}

	// 3. Process entry signals.
	for _, sig := range sigs {
		if sig == nil || sig.Side != Buy {
			continue
		}
		snap, ok := byToken[sig.TokenID]
		if !ok {
			continue
		}
		// Fill at sig.Price when set; fall back to fair price.
		entry := sig.Price
		if entry <= 0 {
			entry = snap.FairPrice
		}
		if entry <= 0 {
			continue
		}

		// Net opposite position before entering, unless disabled (e.g. straddle).
		if !pt.ep.NoNet {
			opp := oppositeOutcome(sig.Outcome)
			for tid, p := range pt.pos {
				if p.outcome != opp {
					continue
				}
				closeAt := p.lastBid
				if closeAt <= 0 {
					closeAt = p.entryPrice
				}
				realized := pt.pnl(p, closeAt)
				pt.bookPnL(realized)
				delete(pt.pos, tid)
				log.Printf("[paper/%s] net flip: closed %s @ %.4f bid  realized=%+.4f  → opening %s",
					name, p.outcome, closeAt, realized, sig.Outcome)
				metrics.PaperWindowPnL.WithLabelValues(name, p.outcome).Set(realized)
				metrics.PaperUnrealizedPnL.WithLabelValues(name, p.outcome).Set(0)
				metrics.PaperTotalPnL.WithLabelValues(name).Set(pt.totalPnL)
				metrics.PaperDailyPnL.WithLabelValues(name).Set(pt.dailyPnL)
			}
		}

		maxRisk := pt.ep.MaxWindowRiskUSDC
		currentNet := pt.netUSDC()
		size := sig.SizeUSDC
		if maxRisk > 0 {
			var room float64
			if sig.Outcome == "Up" {
				room = maxRisk - currentNet
			} else {
				room = maxRisk + currentNet
			}
			if room < pt.ep.MinSizeUSDC {
				continue
			}
			if size > room {
				size = room
			}
		}

		newShares := size / entry
		if existing, ok := pt.pos[sig.TokenID]; ok {
			totalShares := existing.shares + newShares
			existing.entryPrice = (existing.shares*existing.entryPrice + newShares*entry) / totalShares
			existing.shares = totalShares
			existing.cost += size
			existing.lastBid = snap.BestBid
			log.Printf("[paper/%s] ADD %s %.4f @ %.4f fair  +%.2f USDC  total_cost=%.2f  net=%.2f",
				name, sig.Outcome, newShares, entry, size, existing.cost, pt.netUSDC())
		} else {
			pt.pos[sig.TokenID] = &paperPos{
				outcome:    sig.Outcome,
				entryPrice: entry,
				shares:     newShares,
				cost:       size,
				lastBid:    snap.BestBid,
			}
			log.Printf("[paper/%s] BUY %s %.4f @ %.4f fair  cost=%.2f USDC  net=%.2f",
				name, sig.Outcome, newShares, entry, size, pt.netUSDC())
		}

		metrics.PaperTradeEntryPrice.WithLabelValues(name, sig.Outcome, "BUY").Set(pt.pos[sig.TokenID].entryPrice)
		metrics.PaperTradeSize.WithLabelValues(name, sig.Outcome, "BUY").Set(size)
		metrics.PaperTradesTotal.WithLabelValues(name, sig.Outcome, "BUY").Inc()
		metrics.PaperPositionNetUSDC.WithLabelValues(name).Set(pt.netUSDC())
	}
}

// OnExpiry closes any open position for tokenID at settlementVal (0.0 or 1.0),
// books P&L, and returns a WindowRecord. Always returns a record (zeros if flat).
func (pt *PaperTrader) OnExpiry(tokenID, outcome string, settlementVal float64) csvlog.WindowRecord {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	name := pt.sig.Name()
	pt.rollDayIfNeeded()

	rec := csvlog.WindowRecord{
		ClosedAt:        time.Now().UTC(),
		Outcome:         outcome,
		SettlementPrice: settlementVal,
	}

	p, ok := pt.pos[tokenID]
	if !ok {
		rec.DailyPnL = pt.dailyPnL
		rec.TotalPnL = pt.totalPnL
		metrics.PaperUnrealizedPnL.WithLabelValues(name, outcome).Set(0)
		pt.writeWindow(rec)
		return rec
	}

	realized := pt.pnl(p, settlementVal)
	pt.bookPnL(realized)
	delete(pt.pos, tokenID)

	rec.Side = "BUY"
	rec.EntryPrice = p.entryPrice
	rec.Shares = p.shares
	rec.CostUSDC = p.cost
	rec.RealizedPnL = realized
	rec.DailyPnL = pt.dailyPnL
	rec.TotalPnL = pt.totalPnL

	log.Printf("[paper/%s] settled %s @ %.0f  realized=%+.4f  daily=%+.4f  total=%+.4f",
		name, outcome, settlementVal, realized, pt.dailyPnL, pt.totalPnL)

	metrics.PaperWindowPnL.WithLabelValues(name, outcome).Set(realized)
	metrics.PaperUnrealizedPnL.WithLabelValues(name, outcome).Set(0)
	metrics.PaperPositionNetUSDC.WithLabelValues(name).Set(pt.netUSDC())
	metrics.PaperTotalPnL.WithLabelValues(name).Set(pt.totalPnL)
	metrics.PaperDailyPnL.WithLabelValues(name).Set(pt.dailyPnL)

	pt.writeWindow(rec)
	return rec
}

// OnNewWindow resets all open positions (they should have been expired already).
// Call when a new 5-minute window is discovered and the token list is replaced.
func (pt *PaperTrader) OnNewWindow() {
	pt.mu.Lock()
	name := pt.sig.Name()
	if pt.warmup {
		pt.warmup = false
		log.Printf("[paper/%s] warmup complete — orders enabled from next window", name)
	}
	pt.pos = make(map[string]*paperPos)
	pt.mu.Unlock()
	metrics.PaperPositionNetUSDC.WithLabelValues(name).Set(0)
	metrics.PaperWindowRiskUsed.WithLabelValues(name).Set(0)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (pt *PaperTrader) bookPnL(realized float64) {
	pt.dailyPnL += realized
	pt.totalPnL += realized
}

func (pt *PaperTrader) rollDayIfNeeded() {
	today := time.Now().UTC().Format("2006-01-02")
	if today != pt.currentDay {
		pt.dailyPnL = 0
		pt.currentDay = today
		metrics.PaperDailyPnL.WithLabelValues(pt.sig.Name()).Set(0)
	}
}

func (pt *PaperTrader) writeWindow(r csvlog.WindowRecord) {
	if pt.windowLog == nil {
		return
	}
	if err := pt.windowLog.Write(r); err != nil {
		log.Printf("[paper/%s] window log: %v", pt.sig.Name(), err)
	}
}

func (pt *PaperTrader) pnl(p *paperPos, currentMid float64) float64 {
	return p.shares * (currentMid - p.entryPrice)
}
