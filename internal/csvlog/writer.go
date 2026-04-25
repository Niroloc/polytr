package csvlog

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot is one row written to CSV.
type Snapshot struct {
	Ts           time.Time
	MarketID     string
	TokenID      string
	Outcome      string
	Strike       string
	BTCSpot      float64
	BestBid      float64
	BestAsk      float64
	MidPrice     float64
	Spread       float64
	FairPrice    float64
	Edge         float64
	TTESeconds   float64
}

var header = []string{
	"timestamp", "market_id", "token_id", "outcome", "strike",
	"btc_spot", "best_bid", "best_ask", "mid_price", "spread",
	"fair_price", "edge", "tte_seconds",
}

// Writer appends rows to a per-day CSV file.
type Writer struct {
	dir string
	mu  sync.Mutex
	w   *csv.Writer
	f   *os.File
	day string // current file's date YYYY-MM-DD
}

func NewWriter(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Writer{dir: dir}, nil
}

func (w *Writer) Write(s Snapshot) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	day := s.Ts.UTC().Format("2006-01-02")
	if err := w.ensureFile(day); err != nil {
		return err
	}

	row := []string{
		s.Ts.UTC().Format(time.RFC3339Nano),
		s.MarketID,
		s.TokenID,
		s.Outcome,
		s.Strike,
		f(s.BTCSpot),
		f(s.BestBid),
		f(s.BestAsk),
		f(s.MidPrice),
		f(s.Spread),
		f(s.FairPrice),
		f(s.Edge),
		f(s.TTESeconds),
	}
	if err := w.w.Write(row); err != nil {
		return err
	}
	w.w.Flush()
	return w.w.Error()
}

func (w *Writer) ensureFile(day string) error {
	if w.day == day && w.f != nil {
		return nil
	}
	if w.f != nil {
		w.w.Flush()
		w.f.Close()
	}
	path := filepath.Join(w.dir, fmt.Sprintf("polymarket_%s.csv", day))
	needsHeader := !fileExists(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.w = csv.NewWriter(f)
	w.day = day
	if needsHeader {
		return w.w.Write(header)
	}
	return nil
}

func (w *Writer) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.w.Flush()
		w.f.Close()
	}
}

func f(v float64) string { return fmt.Sprintf("%.6f", v) }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
