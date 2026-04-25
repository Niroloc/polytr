package trader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"trading-polymarket/internal/auth"
)

// Client places and cancels orders on the Polymarket CLOB.
type Client struct {
	baseURL    string
	creds      *auth.Credentials
	signer     *Signer
	orderTTL   time.Duration
	httpClient *http.Client
}

func NewClient(baseURL string, creds *auth.Credentials, signer *Signer, orderTTL time.Duration) *Client {
	return &Client{
		baseURL:  baseURL,
		creds:    creds,
		signer:   signer,
		orderTTL: orderTTL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// PlaceOrder signs and submits a limit order.
func (c *Client) PlaceOrder(sig TradeSignal) (*OrderResponse, error) {
	order, err := c.signer.SignOrder(sig, c.orderTTL)
	if err != nil {
		return nil, fmt.Errorf("sign order: %w", err)
	}

	req := OrderRequest{
		Order:     order,
		OrderType: OrderTypeGTD,
		TickSize:  "0.01",
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var resp OrderResponse
	if err := c.post("/order", body, &resp); err != nil {
		return nil, err
	}

	if resp.ErrorMsg != "" {
		return &resp, fmt.Errorf("polymarket order error: %s", resp.ErrorMsg)
	}

	log.Printf("[trader] placed %s %s @ %.4f size=%.2f USDC → orderID=%s status=%s",
		sig.Side, sig.TokenID[:min(8, len(sig.TokenID))],
		sig.Price, sig.SizeUSDC, resp.OrderID, resp.Status)

	return &resp, nil
}

// CancelOrder cancels an open order by its ID.
func (c *Client) CancelOrder(orderID string) error {
	url := fmt.Sprintf("%s/order/%s", c.baseURL, orderID)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.creds.Sign(req, ""); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel order %s: status %d: %s", orderID, resp.StatusCode, string(b))
	}

	log.Printf("[trader] cancelled order %s", orderID)
	return nil
}

func (c *Client) post(path string, body []byte, dst any) error {
	url := c.baseURL + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.creds.Sign(req, string(body)); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
