package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

// A wallet's holding is the sum of its token accounts of the mint, and
// an RPC that fails in any way is an error, never a balance of nought.
func TestRPCBalance(t *testing.T) {
	answer := `{"jsonrpc":"2.0","id":1,"result":{"value":[
		{"account":{"data":{"parsed":{"info":{"tokenAmount":{"amount":"12000000000"}}}}}},
		{"account":{"data":{"parsed":{"info":{"tokenAmount":{"amount":"500000"}}}}}}]}}`
	code := 200
	var asked struct {
		Method string
		Params []json.RawMessage
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&asked)
		w.WriteHeader(code)
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	rpc := &RPC{URL: srv.URL, Mint: "MintMintMint"}
	got, err := rpc.Balance(context.Background(), "OwnerOwnerOwner")
	if err != nil || got != 12000500000 {
		t.Fatalf("balance = %d, %v", got, err)
	}
	if asked.Method != "getTokenAccountsByOwner" || len(asked.Params) != 3 || string(asked.Params[0]) != `"OwnerOwnerOwner"` || string(asked.Params[1]) != `{"mint":"MintMintMint"}` {
		t.Fatalf("asked %s %s", asked.Method, asked.Params)
	}
	answer = `{"jsonrpc":"2.0","id":1,"result":{"value":[]}}`
	if got, err := rpc.Balance(context.Background(), "x"); err != nil || got != 0 {
		t.Fatalf("a wallet with no account of the mint = %d, %v", got, err)
	}
	for name, bad := range map[string]string{
		"an RPC error": `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid param"}}`,
		"not JSON":     `<html>busy</html>`,
		"a bad amount": `{"result":{"value":[{"account":{"data":{"parsed":{"info":{"tokenAmount":{"amount":"-5"}}}}}}]}}`,
		"no result":    `{}`,
	} {
		answer = bad
		if got, err := rpc.Balance(context.Background(), "x"); !errors.Is(err, errRPC) {
			t.Errorf("%s: %d, %v", name, got, err)
		}
	}
	answer, code = `{"jsonrpc":"2.0","id":1,"result":{"value":[]}}`, 429
	if _, err := rpc.Balance(context.Background(), "x"); !errors.Is(err, errRPC) {
		t.Errorf("a 429 was read as a balance: %v", err)
	}
	srv.Close()
	if _, err := rpc.Balance(context.Background(), "x"); !errors.Is(err, errRPC) || err.Error() != errRPC.Error() {
		t.Errorf("an unreachable RPC: %v (the URL may hold a key and must not be said)", err)
	}
}

// The same read against a real RPC, when asked for:
//
//	LIVE_RPC=https://api.mainnet-beta.solana.com LIVE_OWNER=<address> LIVE_MIN=<raw units> go test -run TestLiveRPC ./cmd/exe-stats
func TestLiveRPC(t *testing.T) {
	url, owner := os.Getenv("LIVE_RPC"), os.Getenv("LIVE_OWNER")
	if url == "" || owner == "" {
		t.Skip("LIVE_RPC and LIVE_OWNER not set")
	}
	least, _ := strconv.ParseInt(os.Getenv("LIVE_MIN"), 10, 64)
	got, err := (&RPC{URL: url, Mint: defaults(Config{}).Mint}).Balance(context.Background(), owner)
	if err != nil || got < least {
		t.Fatalf("balance = %d, %v (want at least %d)", got, err, least)
	}
	t.Logf("%s holds %d raw units", owner, got)
}
