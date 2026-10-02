package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"
)

// The holding is read, never moved: one getTokenAccountsByOwner for the
// wallet's accounts of the mint, summed. Nothing here signs anything,
// and the wallet is never asked for a transaction.

type RPC struct {
	URL    string
	Mint   string
	Client *http.Client
}

var errRPC = errors.New("the balance could not be read")

// Balance is the raw units of the mint the address holds, over all of
// its token accounts. An error means the RPC did not answer: the caller
// leaves the account as it was.
func (r *RPC) Balance(ctx context.Context, addr string) (int64, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getTokenAccountsByOwner",
		"params": []any{addr, map[string]string{"mint": r.Mint}, map[string]string{"encoding": "jsonParsed"}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", r.URL, bytes.NewReader(body))
	if err != nil {
		return 0, errRPC
	}
	req.Header.Set("Content-Type", "application/json")
	c := r.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, errRPC // the URL may carry a key: never say it
	}
	defer resp.Body.Close()
	var out struct {
		Error  *struct{ Message string } `json:"error"`
		Result *struct {
			Value []struct {
				Account struct {
					Data struct {
						Parsed struct {
							Info struct {
								TokenAmount struct {
									Amount string `json:"amount"`
								} `json:"tokenAmount"`
							} `json:"info"`
						} `json:"parsed"`
					} `json:"data"`
				} `json:"account"`
			} `json:"value"`
		} `json:"result"`
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("%w: status %d", errRPC, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Result == nil {
		if out.Error != nil {
			return 0, fmt.Errorf("%w: %s", errRPC, out.Error.Message)
		}
		return 0, errRPC
	}
	var sum int64
	for _, v := range out.Result.Value {
		n, err := strconv.ParseUint(v.Account.Data.Parsed.Info.TokenAmount.Amount, 10, 63)
		if err != nil {
			return 0, errRPC
		}
		sum += int64(n)
		if sum < 0 {
			return 0, errRPC
		}
	}
	return sum, nil
}

// ---- base58: a Solana address is its 32-byte ed25519 key in it ----

const b58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Decode(s string) ([]byte, bool) {
	n := new(big.Int)
	radix := big.NewInt(58)
	zeros := 0
	lead := true
	for _, c := range []byte(s) {
		i := bytes.IndexByte([]byte(b58), c)
		if i < 0 {
			return nil, false
		}
		if lead && i == 0 {
			zeros++
		} else {
			lead = false
		}
		n.Mul(n, radix).Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	return append(make([]byte, zeros), b...), true
}

func base58Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	radix, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, b58[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, b58[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// addressKey is the ed25519 key an address names, when it is one.
func addressKey(addr string) ([]byte, bool) {
	if len(addr) < 32 || len(addr) > 44 {
		return nil, false
	}
	k, ok := base58Decode(addr)
	return k, ok && len(k) == 32
}
