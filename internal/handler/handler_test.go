package handler_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smitabharat/wallet-transfer-assignment/internal/handler"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository/pgtest"
	"github.com/smitabharat/wallet-transfer-assignment/internal/service"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := repository.Open(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(handler.New(service.NewTransferService(store, log), service.NewWalletService(store), log))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func mustStatus(t *testing.T, resp *http.Response, body []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d, body: %s", resp.StatusCode, want, body)
	}
}

func setupWallets(t *testing.T, srv *httptest.Server) {
	t.Helper()
	resp, b := do(t, http.MethodPost, srv.URL+"/wallets", `{"id":"wallet_1","initialBalance":1000}`, nil)
	mustStatus(t, resp, b, http.StatusCreated)
	resp, b = do(t, http.MethodPost, srv.URL+"/wallets", `{"id":"wallet_2","initialBalance":0}`, nil)
	mustStatus(t, resp, b, http.StatusCreated)
}

const transferBody = `{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}`

func TestCreateTransferAndReplay(t *testing.T) {
	srv := newServer(t)
	setupWallets(t, srv)

	resp, first := do(t, http.MethodPost, srv.URL+"/transfers", transferBody, nil)
	mustStatus(t, resp, first, http.StatusCreated)
	if resp.Header.Get(handler.HeaderReplayed) != "" {
		t.Error("first response must not be marked as replayed")
	}

	resp, second := do(t, http.MethodPost, srv.URL+"/transfers", transferBody, nil)
	mustStatus(t, resp, second, http.StatusCreated)
	if resp.Header.Get(handler.HeaderReplayed) != "true" {
		t.Error("duplicate response should carry Idempotent-Replayed: true")
	}
	if !bytes.Equal(first, second) {
		t.Errorf("replayed body differs:\n%s\n%s", first, second)
	}

	resp, b := do(t, http.MethodGet, srv.URL+"/wallets/wallet_1", "", nil)
	mustStatus(t, resp, b, http.StatusOK)
	var w struct{ Balance int64 }
	_ = json.Unmarshal(b, &w)
	if w.Balance != 900 {
		t.Errorf("wallet_1 balance = %d, want 900", w.Balance)
	}

	var tr struct{ ID string }
	_ = json.Unmarshal(first, &tr)
	resp, b = do(t, http.MethodGet, srv.URL+"/transfers/"+tr.ID, "", nil)
	mustStatus(t, resp, b, http.StatusOK)
	var details struct {
		LedgerEntries []struct{ Type string } `json:"ledgerEntries"`
	}
	_ = json.Unmarshal(b, &details)
	if len(details.LedgerEntries) != 2 {
		t.Errorf("want 2 ledger entries, got %d", len(details.LedgerEntries))
	}
}

func TestIdempotencyKeyFromHeader(t *testing.T) {
	srv := newServer(t)
	setupWallets(t, srv)
	body := `{"fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}`
	h := map[string]string{handler.HeaderIdempotencyKey: "hdr-1"}

	resp, b := do(t, http.MethodPost, srv.URL+"/transfers", body, h)
	mustStatus(t, resp, b, http.StatusCreated)
	resp, b = do(t, http.MethodPost, srv.URL+"/transfers", body, h)
	mustStatus(t, resp, b, http.StatusCreated)
	if resp.Header.Get(handler.HeaderReplayed) != "true" {
		t.Error("expected replay when key is sent as header")
	}

	resp, b = do(t, http.MethodPost, srv.URL+"/transfers", transferBody, map[string]string{handler.HeaderIdempotencyKey: "other"})
	mustStatus(t, resp, b, http.StatusBadRequest)
}

func TestTransferErrorStatusCodes(t *testing.T) {
	srv := newServer(t)
	setupWallets(t, srv)
	// Consume the key so the conflict case below has something to conflict with.
	resp, b := do(t, http.MethodPost, srv.URL+"/transfers", transferBody, nil)
	mustStatus(t, resp, b, http.StatusCreated)

	cases := map[string]struct {
		body string
		want int
	}{
		"insufficient funds": {`{"idempotencyKey":"big","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100000}`, http.StatusUnprocessableEntity},
		"key conflict":       {`{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":5}`, http.StatusConflict},
		"unknown wallet":     {`{"idempotencyKey":"u","fromWalletId":"wallet_1","toWalletId":"nope","amount":5}`, http.StatusNotFound},
		"negative amount":    {`{"idempotencyKey":"n","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":-5}`, http.StatusBadRequest},
		"same wallet":        {`{"idempotencyKey":"s","fromWalletId":"wallet_1","toWalletId":"wallet_1","amount":5}`, http.StatusBadRequest},
		"malformed json":     {`{"amount":`, http.StatusBadRequest},
		"unknown field":      {`{"amount":5,"fromWalletId":"wallet_1","toWalletId":"wallet_2","extra":1}`, http.StatusBadRequest},
		"fractional amount":  {`{"fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":1.5}`, http.StatusBadRequest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp, b := do(t, http.MethodPost, srv.URL+"/transfers", c.body, nil)
			mustStatus(t, resp, b, c.want)
		})
	}
}

func TestFailedTransferReplaysSameStatus(t *testing.T) {
	srv := newServer(t)
	setupWallets(t, srv)
	body := `{"idempotencyKey":"f1","fromWalletId":"wallet_2","toWalletId":"wallet_1","amount":1}`
	resp, first := do(t, http.MethodPost, srv.URL+"/transfers", body, nil)
	mustStatus(t, resp, first, http.StatusUnprocessableEntity)
	resp, second := do(t, http.MethodPost, srv.URL+"/transfers", body, nil)
	mustStatus(t, resp, second, http.StatusUnprocessableEntity)
	if !bytes.Equal(first, second) {
		t.Errorf("replayed body differs:\n%s\n%s", first, second)
	}
}

func TestWalletEndpoints(t *testing.T) {
	srv := newServer(t)
	setupWallets(t, srv)
	resp, b := do(t, http.MethodPost, srv.URL+"/wallets", `{"id":"wallet_1","initialBalance":5}`, nil)
	mustStatus(t, resp, b, http.StatusConflict)
	resp, b = do(t, http.MethodGet, srv.URL+"/wallets/missing", "", nil)
	mustStatus(t, resp, b, http.StatusNotFound)
	resp, b = do(t, http.MethodPost, srv.URL+"/wallets", `{"id":"neg","initialBalance":-5}`, nil)
	mustStatus(t, resp, b, http.StatusBadRequest)
}
