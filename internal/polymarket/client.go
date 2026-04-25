package polymarket

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetMarket fetches a single market by its condition ID.
func (c *Client) GetMarket(conditionID string) (*Market, error) {
	url := fmt.Sprintf("%s/markets/%s", c.baseURL, conditionID)
	var m Market
	if err := c.get(url, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetOrderBook fetches the current order book for a token (YES or NO side).
func (c *Client) GetOrderBook(tokenID string) (*OrderBook, error) {
	url := fmt.Sprintf("%s/book?token_id=%s", c.baseURL, tokenID)
	var raw apiOrderBookResponse
	if err := c.get(url, &raw); err != nil {
		return nil, err
	}
	return parseOrderBook(raw)
}

// GetMarketByTokenID resolves full market metadata for a YES/NO token ID.
// It reads the order book to get the condition ID, then fetches the market.
// Returns the market, the outcome string ("Yes"/"No"), and any error.
func (c *Client) GetMarketByTokenID(tokenID string) (*Market, string, error) {
	ob, err := c.GetOrderBook(tokenID)
	if err != nil {
		return nil, "", fmt.Errorf("order book: %w", err)
	}
	if ob.MarketID == "" {
		return nil, "", fmt.Errorf("order book returned empty market ID for token %s", tokenID)
	}
	m, err := c.GetMarket(ob.MarketID)
	if err != nil {
		return nil, "", fmt.Errorf("market %s: %w", ob.MarketID, err)
	}
	outcome := ""
	for _, t := range m.Tokens {
		if t.TokenID == tokenID {
			outcome = t.Outcome
			break
		}
	}
	return m, outcome, nil
}

// SearchMarkets searches active BTC binary option markets by keyword.
func (c *Client) SearchMarkets(query string, limit int) ([]Market, error) {
	url := fmt.Sprintf("%s/markets?tag=Bitcoin&limit=%d", c.baseURL, limit)
	var resp struct {
		Data       []Market `json:"data"`
		NextCursor string   `json:"next_cursor"`
	}
	if err := c.get(url, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) get(url string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("polymarket API %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func parseOrderBook(raw apiOrderBookResponse) (*OrderBook, error) {
	ob := &OrderBook{
		MarketID: raw.Market,
		TokenID:  raw.AssetID,
		Ts:       time.Now(),
	}
	for _, b := range raw.Bids {
		l, err := b.toLevel()
		if err != nil {
			return nil, err
		}
		ob.Bids = append(ob.Bids, l)
	}
	for _, a := range raw.Asks {
		l, err := a.toLevel()
		if err != nil {
			return nil, err
		}
		ob.Asks = append(ob.Asks, l)
	}
	return ob, nil
}
