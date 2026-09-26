// Command server runs the wallet transfer HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smitabharat/wallet-transfer-assignment/internal/handler"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository"
	"github.com/smitabharat/wallet-transfer-assignment/internal/service"
)

// defaultDatabaseURL matches the database started by `make db-up`.
const defaultDatabaseURL = "postgres://postgres:postgres@localhost:55432/wallet?sslmode=disable"

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "HTTP listen address")
	dbURL := flag.String("database-url", envOr("DATABASE_URL", defaultDatabaseURL), "PostgreSQL connection string")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	store, err := repository.Open(*dbURL)
	if err != nil {
		log.Error("open database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	api := handler.New(
		service.NewTransferService(store, log),
		service.NewWalletService(store),
		log,
	)
	srv := &http.Server{Addr: *addr, Handler: api, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", slog.String("addr", *addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", slog.String("error", err.Error()))
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", slog.String("error", err.Error()))
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
