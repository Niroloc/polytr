package trader

import (
	"log"
	"math"
	"sync"
	"time"

	"trading-polymarket/internal/csvlog"
	"trading-polymarket/internal/metrics"
)

// PaperTrader simulates strategy execution without real orders.
//
// Position model:
//
//	netUSDC = Σ cost(Up positions) − Σ cost(Down positions)
//	+N means long Up by $N, −N means long Down by $N
//	|netUSDC| ≤ MaxWindowRiskUSDC
//
// A signal fires on every tick where edge ≥ threshold. If a position in the
// same direction is already open, the new size is added (weighted-average entry
// price) up to the budget limit. If a position in the opposite direction is
// open, it is closed first at the last known mid (netting), then the new
// position is opened.
//
// Settlement: at window end every remaining position is closed at 1.0 (ITM)
// or 0.0 (OTM).  P&L = shares × (settlement − entryPrice).
type PaperTrader struct {
	strategy  *Strategy
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
	lastBid    float64 // most recent ask price; used for mark-to-market and mid-window close
}

func NewPaperTrader(p Params, wl *csvlog.WindowWriter) *PaperTrader {
	pt := &PaperTrader{
		strategy:   NewStrategy(p),
		windowLog:  wl,
		pos:        make(map[string]*paperPos),
		warmup:     true,
		currentDay: time.Now().UTC().Format("2006-01-02"),
	}
	metrics.PaperWindowRiskLimit.Set(p.MaxWindowRiskUSDC)
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
// fires on every tick where edge ≥ threshold:
//   - opposite position open → close it first (netting), then enter
//   - same-direction position open → add to it (weighted avg entry) up to limit
//   - no position → open fresh
func (pt *PaperTrader) OnTick(snap Snapshot) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	pt.rollDayIfNeeded()

	// Update lastBid and unrealized P&L (marked at ask — the price we can sell at).
	if p, ok := pt.pos[snap.TokenID]; ok {
		p.lastBid = snap.BestBid
		unrealized := pt.pnl(p, snap.BestBid)
		metrics.PaperUnrealizedPnL.WithLabelValues(p.outcome).Set(unrealized)

		// Close threshold: take profit as soon as edge is gone (<=0);
		// cut a loss only when edge exceeds 1.5× the entry threshold against us.
		closeEdge := 0.0
		if unrealized < 0 {
			closeEdge = -1.5 * pt.strategy.params.EdgeThreshold
		}
		if snap.Edge <= closeEdge {
			pt.bookPnL(unrealized)
			delete(pt.pos, snap.TokenID)

			log.Printf("[paper] close %s @ %.4f ask (edge=%.4f thr=%.4f)  realized=%+.4f  daily=%+.4f",
				p.outcome, snap.BestBid, snap.Edge, closeEdge, unrealized, pt.dailyPnL)

			metrics.PaperWindowPnL.WithLabelValues(p.outcome).Set(unrealized)
			metrics.PaperUnrealizedPnL.WithLabelValues(p.outcome).Set(0)
			metrics.PaperPositionNetUSDC.Set(pt.netUSDC())
			metrics.PaperTotalPnL.Set(pt.totalPnL)
			metrics.PaperDailyPnL.Set(pt.dailyPnL)
			return
		}
	}

	if pt.warmup {
		return // first window is observation-only
	}

	// Evaluate without the openOrders guard — paper trading fires every tick.
	sig := pt.strategyEvalNoGuard(snap)
	if sig == nil || sig.Side != Buy {
		return
	}

	// Maker order: simulate fill at fair price (our limit bid).
	entry := snap.FairPrice
	if entry <= 0 {
		return
	}

	// Close any open position in the opposite direction before entering.
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

		log.Printf("[paper] net flip: closed %s @ %.4f ask  realized=%+.4f  → opening %s",
			p.outcome, closeAt, realized, snap.Outcome)

		metrics.PaperWindowPnL.WithLabelValues(p.outcome).Set(realized)
		metrics.PaperUnrealizedPnL.WithLabelValues(p.outcome).Set(0)
		metrics.PaperTotalPnL.Set(pt.totalPnL)
		metrics.PaperDailyPnL.Set(pt.dailyPnL)
	}

	// How much budget is left in this direction?
	maxRisk := pt.strategy.params.MaxWindowRiskUSDC
	currentNet := pt.netUSDC()
	size := sig.SizeUSDC

	if maxRisk > 0 {
		var room float64
		if snap.Outcome == "Up" {
			room = maxRisk - currentNet
		} else {
			room = maxRisk + currentNet
		}
		if room < pt.strategy.params.MinSizeUSDC {
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

		log.Printf("[paper] ADD %s %.4f @ %.4f fair  +%.2f USDC  total_cost=%.2f  net=%.2f",
			snap.Outcome, newShares, entry, size, existing.cost, pt.netUSDC())
	} else {
		pt.pos[snap.TokenID] = &paperPos{
			outcome:    snap.Outcome,
			entryPrice: entry,
			shares:     newShares,
			cost:       size,
			lastBid:    snap.BestBid,
		}
		log.Printf("[paper] BUY %s %.4f @ %.4f fair  cost=%.2f USDC  net=%.2f",
			snap.Outcome, newShares, entry, size, pt.netUSDC())
	}

	metrics.PaperTradeEntryPrice.WithLabelValues(snap.Outcome, "BUY").Set(pt.pos[snap.TokenID].entryPrice)
	metrics.PaperTradeSize.WithLabelValues(snap.Outcome, "BUY").Set(size)
	metrics.PaperTradesTotal.WithLabelValues(snap.Outcome, "BUY").Inc()
	metrics.PaperPositionNetUSDC.Set(pt.netUSDC())
}

// strategyEvalNoGuard runs the strategy without the "already open" check so
// signals fire every tick while edge is sufficient.
func (pt *PaperTrader) strategyEvalNoGuard(snap Snapshot) *TradeSignal {
	if snap.Expiry.IsZero() || time.Until(snap.Expiry) < 30*time.Second {
		return nil
	}
	p := pt.strategy.params
	if snap.Spread > 3*p.EdgeThreshold {
		return nil
	}
	if snap.MidPrice <= 0 || snap.FairPrice <= 0 {
		return nil
	}
	absEdge := snap.Edge
	if absEdge < 0 {
		absEdge = -absEdge
	}
	if absEdge < p.EdgeThreshold {
		return nil
	}
	// Only BUY signals — no real shorts on Polymarket.
	if snap.Edge <= 0 {
		return nil
	}
	return &TradeSignal{
		TokenID:  snap.TokenID,
		MarketID: snap.MarketID,
		Outcome:  snap.Outcome,
		Side:     Buy,
		Price:    snap.FairPrice, // maker limit at fair value
		SizeUSDC: p.MaxSizeUSDC,
		Expiry:   time.Now().Add(p.OrderTTL),
		Edge:     snap.Edge,
	}
}

// OnExpiry closes any open position for tokenID at settlementVal (0.0 or 1.0),
// books P&L, and returns a WindowRecord. Always returns a record (zeros if flat).
func (pt *PaperTrader) OnExpiry(tokenID, outcome string, settlementVal float64) csvlog.WindowRecord {
	pt.mu.Lock()
	defer pt.mu.Unlock()

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
		metrics.PaperUnrealizedPnL.WithLabelValues(outcome).Set(0)
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

	log.Printf("[paper] settled %s @ %.0f  realized=%+.4f  daily=%+.4f  total=%+.4f",
		outcome, settlementVal, realized, pt.dailyPnL, pt.totalPnL)

	metrics.PaperWindowPnL.WithLabelValues(outcome).Set(realized)
	metrics.PaperUnrealizedPnL.WithLabelValues(outcome).Set(0)
	metrics.PaperPositionNetUSDC.Set(pt.netUSDC())
	metrics.PaperTotalPnL.Set(pt.totalPnL)
	metrics.PaperDailyPnL.Set(pt.dailyPnL)

	pt.writeWindow(rec)
	return rec
}

// OnNewWindow resets all open positions (they should have been expired already).
// Call when a new 5-minute window is discovered and the token list is replaced.
func (pt *PaperTrader) OnNewWindow() {
	pt.mu.Lock()
	if pt.warmup {
		pt.warmup = false
		log.Println("[paper] warmup complete — orders enabled from next window")
	}
	pt.pos = make(map[string]*paperPos)
	pt.mu.Unlock()
	metrics.PaperPositionNetUSDC.Set(0)
	metrics.PaperWindowRiskUsed.Set(0)
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
		metrics.PaperDailyPnL.Set(0)
	}
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
	return p.shares * (currentMid - p.entryPrice)
}

var _ = math.Abs // used in netUSDC room calculation
