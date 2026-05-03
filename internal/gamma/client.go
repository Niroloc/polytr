package gamma

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const baseURL = "https://gamma-api.polymarket.com"
const polymarketBase = "https://polymarket.com"

// MarketInfo holds the resolved data for one BTC up/down market window.
type MarketInfo struct {
	ConditionID string
	Question    string
	EndDate     time.Time
	UpTokenID   string // clobTokenIds[0], outcome "Up"
	DownTokenID string // clobTokenIds[1], outcome "Down"
	Slug        string
}

// Client fetches market metadata from the Polymarket Gamma API.
type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 8 * time.Second}}
}

// Current5m returns the active BTC Up/Down 5-minute market for the current window.
// If the current window isn't live yet, tries the next one.
// Returns nil if neither window is found.
func (c *Client) Current5m() (*MarketInfo, error) {
	now := time.Now().Unix()
	period := int64(300)
	current := (now / period) * period

	for _, ts := range []int64{current, current + period} {
		slug := fmt.Sprintf("btc-updown-5m-%d", ts)
		info, err := c.fetchEvent(slug)
		if err != nil {
			return nil, err
		}
		if info != nil {
			return info, nil
		}
	}
	return nil, nil
}

// DiscoverBTC returns active BTC up/down markets expiring within window.
// Used by -list only.
func (c *Client) DiscoverBTC(window time.Duration) ([]MarketInfo, error) {
	now := time.Now().Unix()
	end := now + int64(window.Seconds())

	var slugs []string
	for _, period := range []int64{300, 900} {
		label := "5m"
		if period == 900 {
			label = "15m"
		}
		start := (now / period) * period
		for t := start; t < end; t += period {
			slugs = append(slugs, fmt.Sprintf("btc-updown-%s-%d", label, t))
		}
	}

	type result struct {
		info *MarketInfo
		err  error
	}
	ch := make(chan result, len(slugs))
	for _, slug := range slugs {
		slug := slug
		go func() {
			info, err := c.fetchEvent(slug)
			ch <- result{info, err}
		}()
	}

	var markets []MarketInfo
	for range slugs {
		r := <-ch
		if r.err == nil && r.info != nil {
			markets = append(markets, *r.info)
		}
	}
	return markets, nil
}

// fetchEvent fetches a single event by slug and returns its MarketInfo.
// Returns (nil, nil) when the event doesn't exist or is closed.
func (c *Client) fetchEvent(slug string) (*MarketInfo, error) {
	url := fmt.Sprintf("%s/events?slug=%s", baseURL, slug)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var events []gammaEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	e := events[0]
	if e.Closed || !e.Active {
		return nil, nil
	}

	// Find the BTC up/down sub-market inside the event.
	for _, m := range e.Markets {
		if !m.Active || m.Closed {
			continue
		}
		ids, outcomes, err := parseClobTokens(m.ClobTokenIDs, m.Outcomes)
		if err != nil || len(ids) < 2 {
			continue
		}
		endDate := parseEndDate(m.EndDate, slug)
		return &MarketInfo{
			ConditionID: m.ConditionID,
			Question:    m.Question,
			EndDate:     endDate,
			UpTokenID:   ids[indexOfOutcome(outcomes, "Up")],
			DownTokenID: ids[indexOfOutcome(outcomes, "Down")],
			Slug:        slug,
		}, nil
	}
	return nil, nil
}

// parseClobTokens decodes the JSON-encoded clobTokenIds string.
func parseClobTokens(raw, outcomesRaw string) ([]string, []string, error) {
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, nil, err
	}
	var outcomes []string
	if err := json.Unmarshal([]byte(outcomesRaw), &outcomes); err != nil {
		return nil, nil, err
	}
	return ids, outcomes, nil
}

func indexOfOutcome(outcomes []string, name string) int {
	for i, o := range outcomes {
		if o == name {
			return i
		}
	}
	return 0 // fallback to first token
}

// parseEndDate tries several common ISO-8601 layouts. If all fail it derives the
// expiry from the slug timestamp (btc-updown-5m-{unix}) + 300 s.
func parseEndDate(raw, slug string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	// Fallback: derive from slug (btc-updown-5m-1234567890 → expiry = ts+300)
	parts := strings.Split(slug, "-")
	if len(parts) > 0 {
		if ts, err := strconv.ParseInt(parts[len(parts)-1], 10, 64); err == nil {
			derived := time.Unix(ts+300, 0).UTC()
			log.Printf("[gamma] warning: could not parse endDate %q, using slug-derived %s", raw, derived.Format(time.RFC3339))
			return derived
		}
	}
	log.Printf("[gamma] warning: could not parse endDate %q and slug fallback failed", raw)
	return time.Time{}
}

// FetchOpenPrice retrieves the Chainlink Data Streams opening price for the
// given slug by fetching the Polymarket event page's React Server Component
// (RSC) payload. This is the exact price Polymarket uses to settle the market.
// Returns 0 on any error (caller should fall back to other sources).
func (c *Client) FetchOpenPrice(slug string) float64 {
	url := fmt.Sprintf("%s/event/%s", polymarketBase, slug)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	// Do NOT set Accept-Encoding manually — http.Transport adds it automatically
	// and handles transparent gzip decompression.
	req.Header.Set("RSC", "1")
	req.Header.Set("Next-Url", "/event/"+slug)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[gamma] FetchOpenPrice %s: %v", slug, err)
		return 0
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 256*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
		if len(buf) > 4*1024*1024 {
			break // safety cap
		}
	}

	windowStart := slugOpenTimeISO(slug)
	p := extractOpenPrice(string(buf), windowStart)
	if p == 0 {
		log.Printf("[gamma] FetchOpenPrice %s: openPrice not found in RSC payload (windowStart=%s)", slug, windowStart)
	}
	return p
}

// slugOpenTimeISO returns the ISO-8601 UTC timestamp for the window open time
// encoded in a slug like "btc-updown-5m-1234567890".
func slugOpenTimeISO(slug string) string {
	parts := strings.Split(slug, "-")
	if len(parts) == 0 {
		return ""
	}
	ts, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// extractOpenPrice parses the RSC payload for a crypto-prices query whose
// queryKey contains windowStart and returns the openPrice field.
func extractOpenPrice(body, windowStart string) float64 {
	if windowStart == "" {
		return 0
	}
	// We search for a "openPrice" value near a queryKey that contains windowStart.
	// The RSC payload contains JSON fragments like:
	//   {"openPrice":78738.84,"closePrice":null}
	//   ...
	//   "queryKey":["crypto-prices","price","BTC","<windowStart>","fiveminute","<windowEnd>"]
	//
	// Strategy: find the queryKey position, then look backwards for openPrice.
	searchKey := `"crypto-prices","price","BTC","` + windowStart + `"`
	idx := strings.Index(body, searchKey)
	if idx < 0 {
		return 0
	}
	// Look backwards up to 800 bytes for "openPrice"
	start := idx - 800
	if start < 0 {
		start = 0
	}
	region := body[start:idx]
	opIdx := strings.LastIndex(region, `"openPrice":`)
	if opIdx < 0 {
		return 0
	}
	raw := region[opIdx+len(`"openPrice":`):]
	// Read digits until non-numeric
	end := 0
	for end < len(raw) && (raw[end] == '.' || (raw[end] >= '0' && raw[end] <= '9')) {
		end++
	}
	v, err := strconv.ParseFloat(raw[:end], 64)
	if err != nil || v == 0 {
		return 0
	}
	return v
}

// ── raw API types ─────────────────────────────────────────────────────────────

type gammaEvent struct {
	Slug    string        `json:"slug"`
	Title   string        `json:"title"`
	Active  bool          `json:"active"`
	Closed  bool          `json:"closed"`
	Markets []gammaMarket `json:"markets"`
}

type gammaMarket struct {
	ConditionID  string `json:"conditionId"`
	Question     string `json:"question"`
	EndDate      string `json:"endDate"`
	ClobTokenIDs string `json:"clobTokenIds"`
	Outcomes     string `json:"outcomes"`
	Active       bool   `json:"active"`
	Closed       bool   `json:"closed"`
}

