package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Evandro-zigzig/payment-ledger-api/internal/ledger"
)

var (
	transfersProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "banking_transfers_settled_total",
		Help: "Total de transferencias liquidadas com sucesso",
	}, []string{"region", "currency"})

	duplicatesBlocked = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "banking_transfers_duplicates_blocked_total",
		Help: "Total de transferencias duplicadas bloqueadas",
	}, []string{"region"})

	transferDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "banking_transfer_duration_seconds",
		Help:    "Latencia total do processamento de transferencias",
		Buckets: prometheus.DefBuckets,
	}, []string{"region"})
)

type transferService interface {
	CreateTransfer(context.Context, ledger.Transfer) (ledger.Transfer, error)
	Check(context.Context) error
}

type transferRequest struct {
	TransactionID string `json:"transaction_id"`
	SourceAccount string `json:"source_account"`
	TargetAccount string `json:"target_account"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
}

type Handler struct {
	service transferService
	region  string
	logger  *slog.Logger
}

func NewHandler(service transferService, region string, logger *slog.Logger) http.Handler {
	handler := &Handler{
		service: service,
		region:  region,
		logger:  logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/transfers", handler.createTransfer)
	mux.HandleFunc("GET /livez", handler.livenessCheck)
	mux.HandleFunc("GET /readyz", handler.healthCheck)
	mux.HandleFunc("GET /healthz", handler.healthCheck)
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

func (h *Handler) createTransfer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		transferDuration.WithLabelValues(h.region).Observe(time.Since(start).Seconds())
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var request transferRequest
	if err := decoder.Decode(&request); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "pedido demasiado grande"}, h.logger)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON invalido"}, h.logger)
		return
	}

	var trailingData any
	if err := decoder.Decode(&trailingData); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "o pedido deve conter um unico objeto JSON"}, h.logger)
		return
	}

	transfer, err := h.service.CreateTransfer(r.Context(), ledger.Transfer{
		TransactionID: request.TransactionID,
		SourceAccount: request.SourceAccount,
		TargetAccount: request.TargetAccount,
		AmountMinor:   request.AmountMinor,
		Currency:      request.Currency,
	})
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrInvalidTransfer):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "campos invalidos; esta versao suporta montantes positivos em USD"}, h.logger)
		case errors.Is(err, ledger.ErrDuplicateTransfer):
			duplicatesBlocked.WithLabelValues(h.region).Inc()
			writeJSON(w, http.StatusConflict, map[string]string{
				"status":  "DUPLICATE_IGNORED",
				"message": "transacao ja processada",
				"region":  h.region,
			}, h.logger)
		case errors.Is(err, ledger.ErrInsufficientBalance):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "saldo de origem insuficiente ou moeda incompativel"}, h.logger)
		case errors.Is(err, ledger.ErrAccountConditionFailed):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "conta de destino inexistente ou moeda incompativel"}, h.logger)
		case errors.Is(err, ledger.ErrWritesDisabled):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "escritas desativadas nesta regiao"}, h.logger)
		default:
			h.logger.Error("falha na transacao DynamoDB", "error", err, "region", h.region)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "falha temporaria na base de dados"}, h.logger)
		}
		return
	}

	transfersProcessed.WithLabelValues(h.region, transfer.Currency).Inc()
	h.logger.Info("transferencia liquidada",
		"transaction_id", transfer.TransactionID,
		"amount_minor", transfer.AmountMinor,
		"currency", transfer.Currency,
		"region", h.region,
	)
	w.Header().Set("X-Processed-Region", h.region)
	writeJSON(w, http.StatusCreated, transfer, h.logger)
}

func (h *Handler) healthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.service.Check(ctx); err != nil {
		if errors.Is(err, ledger.ErrWritesDisabled) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "STANDBY",
				"region": h.region,
			}, h.logger)
			return
		}
		h.logger.Error("health check do DynamoDB falhou", "error", err, "region", h.region)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "DEGRADED",
			"region": h.region,
		}, h.logger)
		return
	}

	w.Header().Set("X-Processed-Region", h.region)
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "HEALTHY",
		"region": h.region,
	}, h.logger)
}

func (h *Handler) livenessCheck(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "LIVE",
		"region": h.region,
	}, h.logger)
}

func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error("falha ao escrever resposta JSON", "error", err)
	}
}
