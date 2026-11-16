package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Evandro-zigzig/payment-ledger-api/internal/ledger"
)

type fakeTransferService struct {
	transfer   ledger.Transfer
	createErr  error
	checkErr   error
	calls      int
	checkCalls int
}

func (s *fakeTransferService) CreateTransfer(_ context.Context, transfer ledger.Transfer) (ledger.Transfer, error) {
	s.calls++
	if s.createErr != nil {
		return ledger.Transfer{}, s.createErr
	}
	transfer.Status = "SETTLED"
	transfer.ProcessedRegion = "us-east-1"
	transfer.CreatedAt = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	s.transfer = transfer
	return transfer, nil
}

func (s *fakeTransferService) Check(context.Context) error {
	s.checkCalls++
	return s.checkErr
}

func testHandler(service *fakeTransferService) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(service, "us-east-1", logger)
}

func TestCreateTransferReturnsCreated(t *testing.T) {
	service := &fakeTransferService{}
	handler := testHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(
		`{"transaction_id":"tx-1","source_account":"source","target_account":"target","amount_minor":1250,"currency":"USD"}`,
	))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Processed-Region") != "us-east-1" {
		t.Fatal("processed region header is missing")
	}
	if service.calls != 1 || service.transfer.AmountMinor != 1250 {
		t.Fatalf("service did not receive expected transfer: %+v", service.transfer)
	}
}

func TestCreateTransferMapsErrors(t *testing.T) {
	tests := []struct {
		name       string
		serviceErr error
		wantStatus int
	}{
		{name: "invalid request", serviceErr: ledger.ErrInvalidTransfer, wantStatus: http.StatusBadRequest},
		{name: "duplicate", serviceErr: ledger.ErrDuplicateTransfer, wantStatus: http.StatusConflict},
		{name: "insufficient balance", serviceErr: ledger.ErrInsufficientBalance, wantStatus: http.StatusUnprocessableEntity},
		{name: "standby write disabled", serviceErr: ledger.ErrWritesDisabled, wantStatus: http.StatusServiceUnavailable},
		{name: "storage unavailable", serviceErr: errors.New("database unavailable"), wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := testHandler(&fakeTransferService{createErr: tt.serviceErr})
			request := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(
				`{"transaction_id":"tx-1","source_account":"source","target_account":"target","amount_minor":1250,"currency":"USD"}`,
			))
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestCreateTransferRejectsMalformedAndOversizedBodies(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "malformed JSON", body: `{`, wantStatus: http.StatusBadRequest},
		{
			name:       "multiple JSON documents",
			body:       `{"transaction_id":"tx-1"} {}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "oversized body",
			body:       `{"transaction_id":"` + strings.Repeat("x", 1<<20) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeTransferService{}
			handler := testHandler(service)
			request := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(tt.body))
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if service.calls != 0 {
				t.Fatalf("service calls = %d, want 0", service.calls)
			}
		})
	}
}

func TestHealthCheck(t *testing.T) {
	service := &fakeTransferService{checkErr: errors.New("database unavailable")}
	handler := testHandler(service)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestLivenessDoesNotDependOnDynamoDB(t *testing.T) {
	service := &fakeTransferService{checkErr: errors.New("database unavailable")}
	handler := testHandler(service)
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if service.checkCalls != 0 {
		t.Fatalf("liveness check called readiness service %d times, want 0", service.checkCalls)
	}
}
