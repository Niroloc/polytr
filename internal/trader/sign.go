package trader

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// CTF Exchange contract addresses by chain ID.
var exchangeContracts = map[int64]string{
	137:   "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E", // Polygon mainnet
	80002: "0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296", // Polygon Amoy testnet
}

// EIP-712 type string for the Order struct (CTF Exchange).
const orderTypeString = "Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)"

// Signer holds the parsed private key and pre-computed domain separator.
type Signer struct {
	key              *ecdsa.PrivateKey
	address          string
	domainSeparator  []byte
	orderTypeHash    []byte
}

// NewSigner parses a hex private key and builds the EIP-712 domain separator.
func NewSigner(hexKey string, chainID int64) (*Signer, error) {
	hexKey = strings.TrimPrefix(hexKey, "0x")
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	contract, ok := exchangeContracts[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported chain ID %d (supported: 137, 80002)", chainID)
	}

	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()
	domSep := buildDomainSeparator("CTF Exchange", "1", chainID, contract)
	typeHash := crypto.Keccak256([]byte(orderTypeString))

	return &Signer{
		key:             key,
		address:         addr,
		domainSeparator: domSep,
		orderTypeHash:   typeHash,
	}, nil
}

// Address returns the Ethereum address derived from the private key.
func (s *Signer) Address() string { return s.address }

// SignOrder builds and signs the order, returning a ready-to-send SignedOrder.
// price is in [0,1] (Polymarket convention); sizeUSDC is the notional.
func (s *Signer) SignOrder(sig TradeSignal, ttl time.Duration) (SignedOrder, error) {
	// Polymarket amounts use 6-decimal USDC and token amounts scaled to 1e6.
	// makerAmount = USDC to spend (for BUY), takerAmount = tokens to receive.
	// For a BUY at price p: spend makerAmount USDC, receive takerAmount tokens
	//   makerAmount = sizeUSDC * 1e6
	//   takerAmount = sizeUSDC / price * 1e6
	const scale = 1_000_000

	var makerAmt, takerAmt *big.Int
	if sig.Side == Buy {
		makerAmt = usdcToWei(sig.SizeUSDC, scale)
		takerAmt = tokenAmount(sig.SizeUSDC, sig.Price, scale)
	} else {
		// SELL: give tokens, receive USDC
		takerAmt = usdcToWei(sig.SizeUSDC, scale)
		makerAmt = tokenAmount(sig.SizeUSDC, sig.Price, scale)
	}

	salt := big.NewInt(rand.Int63()) //nolint:gosec
	expiration := big.NewInt(time.Now().Add(ttl).Unix())
	tokenID, ok := new(big.Int).SetString(sig.TokenID, 10)
	if !ok {
		// token IDs can also be hex strings
		b, err := hex.DecodeString(strings.TrimPrefix(sig.TokenID, "0x"))
		if err != nil {
			return SignedOrder{}, fmt.Errorf("parse tokenId %q: %w", sig.TokenID, err)
		}
		tokenID = new(big.Int).SetBytes(b)
	}

	order := SignedOrder{
		Salt:          salt.String(),
		Maker:         s.address,
		Signer:        s.address,
		Taker:         "0x0000000000000000000000000000000000000000",
		TokenID:       tokenID.String(),
		MakerAmount:   makerAmt.String(),
		TakerAmount:   takerAmt.String(),
		Expiration:    expiration.String(),
		Nonce:         "0",
		FeeRateBps:    "0",
		Side:          sig.Side,
		SignatureType: 0, // EOA
	}

	hash := s.hashOrder(order, tokenID)
	sigBytes, err := crypto.Sign(hash, s.key)
	if err != nil {
		return SignedOrder{}, fmt.Errorf("ecdsa sign: %w", err)
	}
	// EIP-2 / Ethereum: v = 27 or 28
	sigBytes[64] += 27
	order.Signature = "0x" + hex.EncodeToString(sigBytes)

	return order, nil
}

// hashOrder computes the EIP-712 digest for the given order.
func (s *Signer) hashOrder(o SignedOrder, tokenID *big.Int) []byte {
	makerAmt, _ := new(big.Int).SetString(o.MakerAmount, 10)
	takerAmt, _ := new(big.Int).SetString(o.TakerAmount, 10)
	salt, _ := new(big.Int).SetString(o.Salt, 10)
	exp, _ := new(big.Int).SetString(o.Expiration, 10)

	makerAddr := hexToBytes32(o.Maker)
	signerAddr := hexToBytes32(o.Signer)
	takerAddr := hexToBytes32(o.Taker)

	// ABI-encode: typeHash ++ fields in order
	encoded := concat(
		s.orderTypeHash,
		pad32(salt),
		makerAddr,
		signerAddr,
		takerAddr,
		pad32(tokenID),
		pad32(makerAmt),
		pad32(takerAmt),
		pad32(exp),
		pad32(big.NewInt(0)),              // nonce
		pad32(big.NewInt(0)),              // feeRateBps
		pad32(big.NewInt(int64(o.Side))), // side
		pad32(big.NewInt(0)),              // signatureType
	)
	structHash := crypto.Keccak256(encoded)

	// EIP-712 final digest: \x19\x01 ++ domainSeparator ++ structHash
	digest := crypto.Keccak256(
		[]byte("\x19\x01"),
		s.domainSeparator,
		structHash,
	)
	return digest
}

// buildDomainSeparator computes the EIP-712 domain separator.
func buildDomainSeparator(name, version string, chainID int64, contract string) []byte {
	domainTypeHash := crypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	encoded := concat(
		domainTypeHash,
		crypto.Keccak256([]byte(name)),
		crypto.Keccak256([]byte(version)),
		pad32(big.NewInt(chainID)),
		hexToBytes32(contract),
	)
	return crypto.Keccak256(encoded)
}

func pad32(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func hexToBytes32(addr string) []byte {
	addr = strings.TrimPrefix(strings.ToLower(addr), "0x")
	b, _ := hex.DecodeString(addr)
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func usdcToWei(usdc float64, scale int64) *big.Int {
	f := new(big.Float).SetFloat64(usdc * float64(scale))
	i, _ := f.Int(nil)
	return i
}

func tokenAmount(usdc, price float64, scale int64) *big.Int {
	if price <= 0 {
		return big.NewInt(0)
	}
	f := new(big.Float).SetFloat64(usdc / price * float64(scale))
	i, _ := f.Int(nil)
	return i
}
