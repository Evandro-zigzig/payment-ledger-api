package main


import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
    "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)


var (
	transfersProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "banking_transfers_settled_total",
		Help: "Total de transferencias bancarias liquidadas com sucesso",
	}, []string{"region", "currency"})

	duplicatesBlocked = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "banking_transfers_duplicates_blocked_total",
		Help: "Total de tentativas de transacoes duplicadas bloqueadas por idempotencia",
	}, []string{"region"})

	transferDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "banking_transfer_duration_seconds",
		Help:    "Latencia do processamento de transferencias no DynamoDB",
		Buckets: prometheus.DefBuckets,
	}, []string{"region"})
)

type TransferRequest struct {
	TransactionID string  `json:"transaction_id"`
	SourceAccount string  `json:"source_account"`
	TargetAccount string  `json:"target_account"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
}

type TransferRecord struct {
	TransactionID   string  `dynamodbav:"TransactionID" json:"transaction_id"`
	SourceAccount   string  `dynamodbav:"SourceAccount" json:"source_account"`
	TargetAccount   string  `dynamodbav:"TargetAccount" json:"target_account"`
	Amount          float64 `dynamodbav:"Amount" json:"amount"`
	Currency        string  `dynamodbav:"Currency" json:"currency"`
	Status          string  `dynamodbav:"Status" json:"status"`
	ProcessedRegion string  `dynamodbav:"ProcessedRegion" json:"processed_region"`
	CreatedAt       string  `dynamodbav:"CreatedAt" json:"created_at"`
}

type App struct {
	dbClient  *dynamodb.Client
	tableName string
	region    string
	logger    *slog.Logger
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	tableName := os.Getenv("DYNAMODB_TABLE")
	if tableName == "" {
		tableName = "banking-ledger-global"
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		logger.Error("falha ao carregar configuracao AWS", "error", err)
		os.Exit(1)
	}

	app := &App{
		dbClient:  dynamodb.NewFromConfig(cfg),
		tableName: tableName,
		region:    region,
		logger:    logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/transfers", app.handleCreateTransfer)
	mux.HandleFunc("GET /healthz", app.handleHealthCheck)
	mux.Handle("GET /metrics", promhttp.Handler())

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("API bancaria iniciada com observabilidade", "port", 8080, "region", region)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("erro fatal no servidor HTTP", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("a encerrar servidor de forma segura (graceful shutdown)...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("erro forcado ao encerrar servidor", "error", err)
	}
	logger.Info("servidor encerrado com sucesso")
}

func (a *App) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.respondJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON invalido"})
		return
	}

	if req.TransactionID == "" || req.Amount <= 0 || req.SourceAccount == "" || req.TargetAccount == "" {
		a.respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campos obrigatorios em falta ou valor invalido"})
		return
	}

	if req.Currency == "" {
		req.Currency = "USD"
	}

	record := TransferRecord{
		TransactionID:   req.TransactionID,
		SourceAccount:   req.SourceAccount,
		TargetAccount:   req.TargetAccount,
		Amount:          req.Amount,
		Currency:        req.Currency,
		Status:          "SETTLED",
		ProcessedRegion: a.region,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		a.logger.Error("erro ao serializar item", "error", err)
		a.respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "erro interno"})
		return
	}

	_, err = a.dbClient.PutItem(r.Context(), &dynamodb.PutItemInput{
		TableName:           aws.String(a.tableName),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(TransactionID)"),
	})

	transferDuration.WithLabelValues(a.region).Observe(time.Since(start).Seconds())

	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			duplicatesBlocked.WithLabelValues(a.region).Inc()
			a.logger.Warn("transacao duplicada bloqueada por idempotencia", "transaction_id", req.TransactionID)
			a.respondJSON(w, http.StatusConflict, map[string]string{
				"status":  "DUPLICATE_IGNORED",
				"message": "Transacao ja processada anteriormente",
				"region":  a.region,
			})
			return
		}

		a.logger.Error("falha ao gravar no DynamoDB", "error", err, "region", a.region)
		a.respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "falha na base de dados regional"})
		return
	}

	transfersProcessed.WithLabelValues(a.region, record.Currency).Inc()
	a.logger.Info("transacao liquidada com sucesso",
		"transaction_id", record.TransactionID,
		"amount", record.Amount,
		"region", a.region,
	)

	w.Header().Set("X-Processed-Region", a.region)
	a.respondJSON(w, http.StatusCreated, record)
}

func (a *App) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("SIMULATE_OUTAGE") == "true" {
		a.logger.Warn("health check falhou devido a simulacao de desastre")
		a.respondJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "UNHEALTHY",
			"region": a.region,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	_, err := a.dbClient.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(a.tableName),
	})

	if err != nil {
		a.logger.Error("falha no health check do DynamoDB", "error", err, "region", a.region)
		a.respondJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "DEGRADED",
			"region": a.region,
		})
		return
	}

	w.Header().Set("X-Processed-Region", a.region)
	a.respondJSON(w, http.StatusOK, map[string]string{
		"status": "HEALTHY",
		"region": a.region,
	})
}

func (a *App) respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
