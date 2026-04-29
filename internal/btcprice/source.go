package btcprice

import "context"

// Source is anything that can provide a live BTC/USD price.
type Source interface {
	// Run keeps the price updated until ctx is cancelled. Call in a goroutine.
	Run(ctx context.Context)
	// Price returns the most recently received price (0 before first update).
	Price() float64
	// WaitPrice blocks until the first price arrives or ctx is cancelled.
	WaitPrice(ctx context.Context) float64
}
