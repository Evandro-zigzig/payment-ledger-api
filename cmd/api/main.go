package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/Evandro-zigzig/payment-ledger-api/internal/httpapi"
	"github.com/Evandro-zigzig/payment-ledger-api/internal/ledger"
)

func main() {
	if err := run(); err != nil {
		slog.Error("API terminou com erro", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	tableName := os.Getenv("DYNAMODB_TABLE")
	if tableName == "" {
		return errors.New("DYNAMODB_TABLE is required")
	}
	writesSetting := os.Getenv("WRITES_ENABLED")
	if writesSetting != "true" && writesSetting != "false" {
		return errors.New("WRITES_ENABLED must be explicitly set to true or false")
	}
	writesEnabled := writesSetting == "true"

	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}

	dynamoClient := dynamodb.NewFromConfig(awsConfig)
	store := ledger.NewDynamoDBStore(dynamoClient, tableName)
	service := ledger.NewService(store, region, writesEnabled)
	handler := httpapi.NewHandler(service, region, logger)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("API do NovaBanco iniciada", "address", server.Addr, "region", region)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
	case <-ctx.Done():
		logger.Info("a encerrar a API de forma graciosa")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			closeErr := server.Close()
			if closeErr != nil {
				logger.Error("erro ao forcar o encerramento do servidor", "error", closeErr)
			}
			return fmt.Errorf("graceful shutdown: %w", err)
		}

		if err := <-serverErrors; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server stopped unexpectedly after shutdown: %w", err)
		}
	}

	return nil
}
