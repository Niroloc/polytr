package trader

import "time"

type Side int

const (
	Buy  Side = 0
	Sell Side = 1
)

func (s Side) String() string {
	if s == Buy {
		return "BUY"
	}
	return "SELL"
}

// OrderType mirrors Polymarket CLOB order types.
type OrderType string

const (
	OrderTypeGTD OrderType = "GTD" // Good Till Date — expires at Expiration
	OrderTypeGTC OrderType = "GTC" // Good Till Cancelled
	OrderTypeFOK OrderType = "FOK" // Fill Or Kill
)

// SignedOrder is the payload sent to POST /order.
// Fields are strings because the API expects decimal strings for big integers.
type SignedOrder struct {
	Salt          string `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          Side   `json:"side"`
	SignatureType int    `json:"signatureType"` // 0 = EOA
	Signature     string `json:"signature"`
}

// OrderRequest wraps the signed order and metadata for the REST call.
type OrderRequest struct {
	Order     SignedOrder `json:"order"`
	OrderType OrderType  `json:"orderType"`
	// TickSize is required by the API; Polymarket uses 0.01 for most markets.
	TickSize string `json:"tickSize"`
}

// OrderResponse is returned by POST /order.
type OrderResponse struct {
	OrderID         string  `json:"orderID"`
	TransactionHash string  `json:"transactionHash,omitempty"`
	Status          string  `json:"status"`
	ErrorMsg        string  `json:"errorMsg,omitempty"`
}

// TradeSignal is produced by the strategy and consumed by the trader.
type TradeSignal struct {
	TokenID  string
	MarketID string
	Outcome  string
	Side     Side
	Price    float64 // limit price in USDC (0–1 scale, e.g. 0.54)
	SizeUSDC float64 // notional USDC to spend/receive
	Expiry   time.Time
	Edge     float64
}
