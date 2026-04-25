package btcprice

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

// Feed maintains the latest BTC/USDT price from Binance WebSocket.
type Feed struct {
	url  string
	pair string
	// atomic pointer to float64; avoids locks on the hot read path
	price unsafe.Pointer
}

func NewFeed(wsURL, pair string) *Feed {
	initial := float64(0)
	f := &Feed{url: wsURL, pair: pair}
	atomic.StorePointer(&f.price, unsafe.Pointer(&initial))
	return f
}

// Price returns the most recently received trade price.
func (f *Feed) Price() float64 {
	p := (*float64)(atomic.LoadPointer(&f.price))
	return *p
}

// WaitPrice blocks until the first trade price arrives or ctx is cancelled.
func (f *Feed) WaitPrice(ctx context.Context) float64 {
	for {
		if p := f.Price(); p > 0 {
			return p
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Run connects to Binance and keeps the price updated.
// Reconnects automatically on error. Blocks until ctx is cancelled.
func (f *Feed) Run(ctx context.Context) {
	streamURL := fmt.Sprintf("%s/%s@trade", f.url, f.pair)
	for {
		if err := f.connect(ctx, streamURL); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[btcprice] reconnecting after error: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func (f *Feed) connect(ctx context.Context, url string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Printf("[btcprice] connected to %s", url)

	type tradeMsg struct {
		Price string `json:"p"` // trade price
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var t tradeMsg
		if err := json.Unmarshal(msg, &t); err != nil {
			continue
		}
		p, err := strconv.ParseFloat(t.Price, 64)
		if err != nil {
			continue
		}
		atomic.StorePointer(&f.price, unsafe.Pointer(&p))
	}
}
