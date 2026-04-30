package btcprice

import (
	"context"
	"log"
	"math"
	"sync"
	"time"
)

const volSampleInterval = 3 * time.Second

// samplesPerYear for the fixed 3-second sampling cadence.
var samplesPerYear = float64(365*24*time.Hour) / float64(volSampleInterval)

// RollingVol computes realized annualized volatility from a rolling window of
// BTC price samples taken from a Binance feed.  Sigma() returns 0 until the
// window holds at least 2 observations; callers should fall back to a static σ.
type RollingVol struct {
	src    Source
	window time.Duration

	mu     sync.Mutex
	points []volPoint
	sigma  float64 // latest estimate; 0 = insufficient data
}

type volPoint struct {
	t  time.Time
	px float64
}

func NewRollingVol(src Source, window time.Duration) *RollingVol {
	return &RollingVol{src: src, window: window}
}

// Sigma returns the current annualized realized volatility, or 0 if there are
// fewer than 2 samples in the window.
func (rv *RollingVol) Sigma() float64 {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	return rv.sigma
}

// Run samples the source every 3 seconds and recomputes volatility.
// Blocks until ctx is cancelled.
func (rv *RollingVol) Run(ctx context.Context) {
	log.Printf("[sigma] rolling vol started, window=%s", rv.window)
	ticker := time.NewTicker(volSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if p := rv.src.Price(); p > 0 {
				rv.record(p)
			}
		}
	}
}

func (rv *RollingVol) record(px float64) {
	rv.mu.Lock()
	defer rv.mu.Unlock()

	now := time.Now()
	rv.points = append(rv.points, volPoint{t: now, px: px})

	// Prune observations outside the rolling window.
	cutoff := now.Add(-rv.window)
	keep := 0
	for keep < len(rv.points) && rv.points[keep].t.Before(cutoff) {
		keep++
	}
	rv.points = rv.points[keep:]

	n := len(rv.points)
	if n < 2 {
		rv.sigma = 0
		return
	}

	// Log returns between consecutive samples.
	var sum, sumSq float64
	for i := 1; i < n; i++ {
		r := math.Log(rv.points[i].px / rv.points[i-1].px)
		sum += r
		sumSq += r * r
	}
	m := float64(n - 1) // number of returns
	// Sample variance with Bessel's correction.
	variance := (sumSq - sum*sum/m) / (m - 1)
	if m < 2 || variance <= 0 {
		rv.sigma = 0
		return
	}

	rv.sigma = math.Sqrt(variance) * math.Sqrt(samplesPerYear)
}
