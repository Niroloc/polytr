package csvlog

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WindowRecord summarises the result of one 5-minute paper-trading window
// for a single outcome (Up or Down).
type WindowRecord struct {
	ClosedAt    time.Time
	Outcome     string
	Side        string  // "BUY", "SELL", or "" if no trade was taken
	EntryPrice  float64 // 0 if no trade
	ExitPrice   float64 // last mid price at expiry; 0 if no trade
	Contracts   float64 // signed: +N long, −N short; 0 if no trade
	CostUSDC    float64 // USDC at risk; 0 if no trade
	RealizedPnL float64 // P&L for this outcome this window
	DailyPnL    float64 // running P&L for the current calendar day (UTC)
	TotalPnL    float64 // all-time cumulative P&L
}

var windowHeader = []string{
	"timestamp", "outcome", "side",
	"entry_price", "exit_price", "contracts", "cost_usdc",
	"realized_pnl", "daily_pnl", "total_pnl",
}

// WindowWriter appends window summary rows to a per-day CSV file.
type WindowWriter struct {
	dir string
	mu  sync.Mutex
	w   *csv.Writer
	f   *os.File
	day string
}

func NewWindowWriter(dir string) (*WindowWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &WindowWriter{dir: dir}, nil
}

func (ww *WindowWriter) Write(r WindowRecord) error {
	ww.mu.Lock()
	defer ww.mu.Unlock()

	day := r.ClosedAt.UTC().Format("2006-01-02")
	if err := ww.ensureFile(day); err != nil {
		return err
	}

	row := []string{
		r.ClosedAt.UTC().Format(time.RFC3339),
		r.Outcome,
		r.Side,
		f(r.EntryPrice),
		f(r.ExitPrice),
		f(r.Contracts),
		f(r.CostUSDC),
		f(r.RealizedPnL),
		f(r.DailyPnL),
		f(r.TotalPnL),
	}
	if err := ww.w.Write(row); err != nil {
		return err
	}
	ww.w.Flush()
	return ww.w.Error()
}

func (ww *WindowWriter) Close() {
	ww.mu.Lock()
	defer ww.mu.Unlock()
	if ww.f != nil {
		ww.w.Flush()
		ww.f.Close()
	}
}

func (ww *WindowWriter) ensureFile(day string) error {
	if ww.day == day && ww.f != nil {
		return nil
	}
	if ww.f != nil {
		ww.w.Flush()
		ww.f.Close()
	}
	path := filepath.Join(ww.dir, fmt.Sprintf("windows_%s.csv", day))
	needsHeader := !fileExists(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	ww.f = file
	ww.w = csv.NewWriter(file)
	ww.day = day
	if needsHeader {
		return ww.w.Write(windowHeader)
	}
	return nil
}
