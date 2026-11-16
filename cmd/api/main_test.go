package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDynamoDB struct {
	putItemErr       error
	describeTableErr error
	putItemInput     *dynamodb.PutItemInput
	putItemCalls     int
}

func (f *fakeDynamoDB) PutItem(_ context.Context, input *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putItemCalls++
	f.putItemInput = input
	return &dynamodb.PutItemOutput{}, f.putItemErr
}

func (f *fakeDynamoDB) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, f.describeTableErr
}

func newTestApp(db *fakeDynamoDB) *App {
	return &App{
		dbClient:  db,
		tableName: "test-ledger",
		region:    "test-region",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestHandleCreateTransfer(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		dbErr      error
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "creates transfer record",
			body:       `{"transaction_id":"tx-1","source_account":"from","target_account":"to","amount":12.5}`,
			wantStatus: http.StatusCreated,
			wantCalls:  1,
		},
		{
			name:       "rejects duplicate transaction",
			body:       `{"transaction_id":"tx-1","source_account":"from","target_account":"to","amount":12.5}`,
			dbErr:      &types.ConditionalCheckFailedException{},
			wantStatus: http.StatusConflict,
			wantCalls:  1,
		},
		{
			name:       "returns unavailable when database fails",
			body:       `{"transaction_id":"tx-1","source_account":"from","target_account":"to","amount":12.5}`,
			dbErr:      errors.New("database unavailable"),
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  1,
		},
		{
			name:       "rejects malformed JSON",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "rejects missing required fields",
			body:       `{"transaction_id":" ","source_account":"from","target_account":"to","amount":12.5}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "rejects non-positive amount",
			body:       `{"transaction_id":"tx-1","source_account":"from","target_account":"to","amount":0}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "rejects multiple JSON values",
			body:       `{"transaction_id":"tx-1","source_account":"from","target_account":"to","amount":12.5} {}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "rejects oversized body",
			body:       `{"transaction_id":"` + strings.Repeat("x", 1<<20) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeDynamoDB{putItemErr: tt.dbErr}
			app := newTestApp(db)
			req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(tt.body))
			recorder := httptest.NewRecorder()

			app.handleCreateTransfer(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if db.putItemCalls != tt.wantCalls {
				t.Fatalf("PutItem calls = %d, want %d", db.putItemCalls, tt.wantCalls)
			}

			if tt.name == "creates transfer record" {
				if got := recorder.Header().Get("X-Processed-Region"); got != "test-region" {
					t.Errorf("X-Processed-Region = %q, want %q", got, "test-region")
				}
				if db.putItemInput == nil || db.putItemInput.TableName == nil || *db.putItemInput.TableName != "test-ledger" {
					t.Error("PutItem was not called with the configured table")
				}
				if db.putItemInput != nil && db.putItemInput.ConditionExpression != nil {
					if got := *db.putItemInput.ConditionExpression; got != "attribute_not_exists(TransactionID)" {
						t.Errorf("condition expression = %q, want idempotency condition", got)
					}
				} else {
					t.Error("PutItem was not called with an idempotency condition")
				}
				if !strings.Contains(recorder.Body.String(), `"currency":"USD"`) {
					t.Errorf("missing default currency in response: %s", recorder.Body.String())
				}
			}
		})
	}
}

func TestHandleHealthCheck(t *testing.T) {
	tests := []struct {
		name       string
		dbErr      error
		wantStatus int
	}{
		{name: "healthy database", wantStatus: http.StatusOK},
		{name: "database unavailable", dbErr: errors.New("database unavailable"), wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(&fakeDynamoDB{describeTableErr: tt.dbErr})
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			recorder := httptest.NewRecorder()

			app.handleHealthCheck(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}
