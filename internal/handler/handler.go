// Package handler is the HTTP transport: it decodes and validates requests,
// calls the service layer and maps results and errors to HTTP responses.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
	"github.com/smitabharat/wallet-transfer-assignment/internal/service"
)

// TransferService is the subset of the service layer used by the handler.
type TransferService interface {
	CreateTransfer(ctx context.Context, req domain.TransferRequest) (service.TransferResult, error)
	GetTransfer(ctx context.Context, id string) (service.TransferDetails, error)
}

// WalletService is the subset of the wallet service used by the handler.
type WalletService interface {
	CreateWallet(ctx context.Context, id string, initialBalance int64) (domain.Wallet, error)
	GetWallet(ctx context.Context, id string) (domain.Wallet, error)
}

// Header names used by the transfer API.
const (
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderReplayed       = "Idempotent-Replayed"
)

const maxBodyBytes = 1 << 16

// Handler serves the HTTP API.
type Handler struct {
	transfers TransferService
	wallets   WalletService
	log       *slog.Logger
}

// New builds the HTTP handler with all routes and middleware.
func New(transfers TransferService, wallets WalletService, log *slog.Logger) http.Handler {
	h := &Handler{transfers: transfers, wallets: wallets, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /transfers", h.createTransfer)
	mux.HandleFunc("GET /transfers/{id}", h.getTransfer)
	mux.HandleFunc("POST /wallets", h.createWallet)
	mux.HandleFunc("GET /wallets/{id}", h.getWallet)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return h.logRequests(mux)
}

type createTransferRequest struct {
	IdempotencyKey string `json:"idempotencyKey"`
	FromWalletID   string `json:"fromWalletId"`
	ToWalletID     string `json:"toWalletId"`
	Amount         int64  `json:"amount"`
}

func (h *Handler) createTransfer(w http.ResponseWriter, r *http.Request) {
	var body createTransferRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := body.IdempotencyKey
	if headerKey := r.Header.Get(HeaderIdempotencyKey); headerKey != "" {
		if key != "" && key != headerKey {
			writeError(w, http.StatusBadRequest, "idempotencyKey in body and Idempotency-Key header differ")
			return
		}
		key = headerKey
	}

	res, err := h.transfers.CreateTransfer(r.Context(), domain.TransferRequest{
		IdempotencyKey: key,
		FromWalletID:   body.FromWalletID,
		ToWalletID:     body.ToWalletID,
		Amount:         body.Amount,
	})
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	if res.Replayed {
		w.Header().Set(HeaderReplayed, "true")
	}
	writeJSON(w, transferStatusCode(res.Transfer.Status), res.Transfer)
}

// transferStatusCode is a pure function of the transfer state, so a replayed
// transfer always gets the same status code as the original response.
func transferStatusCode(s domain.TransferStatus) int {
	switch s {
	case domain.TransferProcessed:
		return http.StatusCreated
	case domain.TransferFailed:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusAccepted
	}
}

func (h *Handler) getTransfer(w http.ResponseWriter, r *http.Request) {
	details, err := h.transfers.GetTransfer(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

type createWalletRequest struct {
	ID             string `json:"id"`
	InitialBalance int64  `json:"initialBalance"`
}

func (h *Handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var body createWalletRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wallet, err := h.wallets.CreateWallet(r.Context(), body.ID, body.InitialBalance)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wallet)
}

func (h *Handler) getWallet(w http.ResponseWriter, r *http.Request) {
	wallet, err := h.wallets.GetWallet(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wallet)
}

func (h *Handler) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrWalletNotFound), errors.Is(err, domain.ErrTransferNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrIdempotencyConflict), errors.Is(err, domain.ErrWalletExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "service busy, retry with the same idempotency key")
	default:
		h.log.Error("internal error", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (h *Handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h.log.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)))
	})
}
