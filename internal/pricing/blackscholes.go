package pricing

import (
	"math"
	"time"
)

// BinaryCallPrice returns the fair value of a cash-or-nothing binary call option
// using the Black-Scholes formula: N(d2), where the option pays $1 if S_T > K.
//
// Parameters:
//   S   – current spot price of BTC
//   K   – strike price
//   T   – time to expiry (in years); use TimeToExpiry() helper
//   σ   – annualised volatility (e.g. 0.80 for 80%)
//   r   – risk-free rate (use 0 for sub-hour options)
//
// Returns a probability in [0, 1].
func BinaryCallPrice(S, K, T, sigma, r float64) float64 {
	if T <= 0 || S <= 0 || K <= 0 || sigma <= 0 {
		if S > K {
			return 1.0
		}
		return 0.0
	}
	d2 := (math.Log(S/K) + (r-0.5*sigma*sigma)*T) / (sigma * math.Sqrt(T))
	return math.Exp(-r*T) * normCDF(d2)
}

// BinaryPutPrice returns the fair value of a cash-or-nothing binary put option
// (pays $1 if S_T < K).
func BinaryPutPrice(S, K, T, sigma, r float64) float64 {
	return math.Exp(-r*T) - BinaryCallPrice(S, K, T, sigma, r)
}

// TimeToExpiry converts an absolute expiry time to years from now.
func TimeToExpiry(expiry time.Time) float64 {
	remaining := time.Until(expiry).Seconds()
	if remaining < 0 {
		return 0
	}
	return remaining / (365.25 * 24 * 3600)
}

// normCDF is the standard normal cumulative distribution function.
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}
