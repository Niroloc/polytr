package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Credentials holds the Polymarket L2 API key set.
type Credentials struct {
	Address    string
	APIKey     string
	APISecret  string
	Passphrase string
}

// Sign attaches Polymarket L2 authentication headers to the request.
// Must be called after the request body is set (body affects the signature).
//
// Header scheme (from Polymarket CLOB API docs):
//   POLY-ADDRESS      wallet address
//   POLY-API-KEY      API key
//   POLY-PASSPHRASE   API passphrase
//   POLY-TIMESTAMP    unix timestamp (seconds)
//   POLY-SIGNATURE    base64( HMAC-SHA256(secret, ts + METHOD + path + body) )
func (c *Credentials) Sign(req *http.Request, body string) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := ts + req.Method + req.URL.RequestURI() + body

	sig, err := hmacB64(c.APISecret, msg)
	if err != nil {
		return fmt.Errorf("l2 sign: %w", err)
	}

	req.Header.Set("POLY-ADDRESS", c.Address)
	req.Header.Set("POLY-API-KEY", c.APIKey)
	req.Header.Set("POLY-PASSPHRASE", c.Passphrase)
	req.Header.Set("POLY-TIMESTAMP", ts)
	req.Header.Set("POLY-SIGNATURE", sig)
	return nil
}

func hmacB64(secret, message string) (string, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(message)); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}
