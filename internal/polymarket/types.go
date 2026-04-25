package polymarket

import (
	"strconv"
	"time"
)

type Market struct {
	ConditionID string  `json:"condition_id"`
	QuestionID  string  `json:"question_id"`
	Question    string  `json:"question"`
	Description string  `json:"description"`
	Tokens      []Token `json:"tokens"`
	Active      bool    `json:"active"`
	Closed      bool    `json:"closed"`
	EndDateISO  string  `json:"end_date_iso"`
}

func (m *Market) Expiry() (time.Time, error) {
	return time.Parse(time.RFC3339, m.EndDateISO)
}

type Token struct {
	TokenID  string `json:"token_id"`
	Outcome  string `json:"outcome"` // "Yes" or "No"
	Price    float64
	Winner   bool `json:"winner"`
}

type OrderBookLevel struct {
	Price float64
	Size  float64
}

type OrderBook struct {
	MarketID string
	TokenID  string
	Bids     []OrderBookLevel
	Asks     []OrderBookLevel
	Ts       time.Time
}

func (ob *OrderBook) BestBid() float64 {
	if len(ob.Bids) == 0 {
		return 0
	}
	return ob.Bids[0].Price
}

func (ob *OrderBook) BestAsk() float64 {
	if len(ob.Asks) == 0 {
		return 0
	}
	return ob.Asks[0].Price
}

func (ob *OrderBook) MidPrice() float64 {
	bid, ask := ob.BestBid(), ob.BestAsk()
	if bid == 0 || ask == 0 {
		return 0
	}
	return (bid + ask) / 2
}

func (ob *OrderBook) Spread() float64 {
	return ob.BestAsk() - ob.BestBid()
}

// raw API response structures

type apiOrderBookResponse struct {
	Market  string          `json:"market"`
	AssetID string          `json:"asset_id"`
	Bids    []apiBookLevel  `json:"bids"`
	Asks    []apiBookLevel  `json:"asks"`
	Hash    string          `json:"hash"`
	Ts      string          `json:"timestamp"`
}

type apiBookLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

func (l apiBookLevel) toLevel() (OrderBookLevel, error) {
	p, err := strconv.ParseFloat(l.Price, 64)
	if err != nil {
		return OrderBookLevel{}, err
	}
	s, err := strconv.ParseFloat(l.Size, 64)
	if err != nil {
		return OrderBookLevel{}, err
	}
	return OrderBookLevel{Price: p, Size: s}, nil
}
