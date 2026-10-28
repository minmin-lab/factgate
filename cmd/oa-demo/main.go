package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	// OA_BUILTIN_USERS=off leaves out alice, bob and dana (the demo stack offers
	// only its role accounts); their passwords are then not read.
	omitBuiltin := env("OA_BUILTIN_USERS", "on") == "off"
	builtinPassword := func(key string) string {
		if omitBuiltin {
			return ""
		}
		return requiredEnv(key)
	}
	danaPassword := ""
	if !omitBuiltin {
		danaPassword = env("OA_DANA_PASSWORD", "")
	}
	// OA_LABELS_FILE gives display names for Catalog identifiers; the account
	// switcher is offered only when the role accounts share one password
	// (OA_BUILTIN_USERS=off with OA_SHARED_PASSWORD), KB1-LOOP-147.
	labels, err := oademo.LoadLabels(env("OA_LABELS_FILE", ""))
	if err != nil {
		logger.Error("load OA labels", "error", err)
		os.Exit(1)
	}
	oa, err := oademo.New(oademo.Config{
		ServiceToken:     requiredEnv("OA_SERVICE_TOKEN"),
		CallbackSecret:   requiredEnv("OA_CALLBACK_SECRET"),
		SessionSecret:    requiredEnv("OA_SESSION_SECRET"),
		CallbackURL:      requiredEnv("GATEWAY_CALLBACK_URL"),
		PublicBaseURL:    env("PUBLIC_OA_BASE_URL", "http://127.0.0.1:8092"),
		AlicePassword:    builtinPassword("OA_ALICE_PASSWORD"),
		BobPassword:      builtinPassword("OA_BOB_PASSWORD"),
		DanaPassword:     danaPassword,
		ExtraUsers:       extraUsersFromEnv(),
		OmitBuiltinUsers: omitBuiltin,
		AllowSwitch:      omitBuiltin && os.Getenv("OA_SHARED_PASSWORD") != "",
		DefaultAccount:   env("OA_DEFAULT_ACCOUNT", ""),
		// OA_AGENT_IDENTITY_URL is TaskGate's agent-identity endpoint
		// (KB1-LOOP-151); in the shared-password mode it adds the
		// navigation's "my agent" line. Empty leaves it out.
		AgentIdentityURL: env("OA_AGENT_IDENTITY_URL", ""),
		Labels:           labels,
		ReceiptSigner:    receiptSigner,
		Logger:           logger,
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

// extraUsersFromEnv reads OA_EXTRA_USERS ("name:role[:display name]", comma
// separated). Each password comes from OA_<NAME>_PASSWORD (upper case, "-" and
// "." as "_"); an account without one uses OA_SHARED_PASSWORD, so the demo's
// role accounts can share one password. A missing password stops the demo.
func extraUsersFromEnv() []oademo.ExtraUser {
	var users []oademo.ExtraUser
	for _, entry := range strings.Split(os.Getenv("OA_EXTRA_USERS"), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, ":", 3)
		name, role, display := strings.TrimSpace(fields[0]), "", ""
		if len(fields) > 1 {
			role = strings.TrimSpace(fields[1])
		}
		if len(fields) > 2 {
			display = strings.TrimSpace(fields[2])
		}
		key := "OA_" + strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(name)) + "_PASSWORD"
		password := os.Getenv(key)
		if password == "" {
			password = os.Getenv("OA_SHARED_PASSWORD")
		}
		if password == "" {
			password = requiredEnv(key)
		}
		users = append(users, oademo.ExtraUser{Name: name, Role: role, Password: password, DisplayName: display})
	}
	return users
}
