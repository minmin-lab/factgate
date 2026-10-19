package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/oademo"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	receiptSigner, err := approval.NewReceiptSignerFromBase64(
		requiredEnv("OA_RECEIPT_KEY_ID"), requiredEnv("OA_RECEIPT_PRIVATE_KEY"),
	)
	if err != nil {
		logger.Error("initialize OA approval receipt signer", "error", err)
		os.Exit(1)
	}
	oa, err := oademo.New(oademo.Config{
		ServiceToken:   requiredEnv("OA_SERVICE_TOKEN"),
		CallbackSecret: requiredEnv("OA_CALLBACK_SECRET"),
		SessionSecret:  requiredEnv("OA_SESSION_SECRET"),
		CallbackURL:    requiredEnv("GATEWAY_CALLBACK_URL"),
		PublicBaseURL:  env("PUBLIC_OA_BASE_URL", "http://127.0.0.1:8092"),
		AlicePassword:  requiredEnv("OA_ALICE_PASSWORD"),
		BobPassword:    requiredEnv("OA_BOB_PASSWORD"),
		DanaPassword:   env("OA_DANA_PASSWORD", ""),
		ReceiptSigner:  receiptSigner,
		Logger:         logger,
	})
	if err != nil {
		logger.Error("initialize OA demo", "error", err)
		os.Exit(1)
	}
	server := &http.Server{Addr: env("OA_ADDR", ":8092"), Handler: oa.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Info("oa demo listening", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("oa demo stopped", "error", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func requiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		slog.Error("required environment variable is missing", "name", key)
		os.Exit(1)
	}
	return value
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
