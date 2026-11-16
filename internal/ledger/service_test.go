package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeTransferStore struct {
	transfer Transfer
	err      error
	calls    int
}

func (s *fakeTransferStore) CreateTransfer(_ context.Context, transfer Transfer) error {
	s.calls++
	s.transfer = transfer
	return s.err
}

func (s *fakeTransferStore) Check(context.Context) error {
	return nil
}

func TestServiceCreateTransfer(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.FixedZone("test", 3600))
	store := &fakeTransferStore{}
	service := NewService(store, "us-east-1", true)
	service.now = func() time.Time { return now }

	transfer, err := service.CreateTransfer(context.Background(), Transfer{
		TransactionID: " tx-1 ",
		SourceAccount: " account-a ",
		TargetAccount: "account-b",
		AmountMinor:   1250,
		Currency:      "USD",
	})
	if err != nil {
		t.Fatalf("CreateTransfer() error = %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
	if transfer.TransactionID != "tx-1" || transfer.SourceAccount != "account-a" {
		t.Fatalf("transfer fields were not normalized: %+v", transfer)
	}
	if transfer.Currency != "USD" || transfer.AmountMinor != 1250 {
		t.Fatalf("unexpected amount or currency: %+v", transfer)
	}
	if transfer.Status != "SETTLED" || transfer.ProcessedRegion != "us-east-1" {
		t.Fatalf("unexpected settlement metadata: %+v", transfer)
	}
	if !transfer.CreatedAt.Equal(now.UTC()) {
		t.Fatalf("CreatedAt = %s, want %s", transfer.CreatedAt, now.UTC())
	}
}

func TestServiceRejectsInvalidTransferWithoutCallingStore(t *testing.T) {
	tests := []struct {
		name     string
		transfer Transfer
	}{
		{
			name: "non-positive amount",
			transfer: Transfer{
				TransactionID: "tx-1", SourceAccount: "a", TargetAccount: "b",
				AmountMinor: 0, Currency: "USD",
			},
		},
		{
			name: "same source and target",
			transfer: Transfer{
				TransactionID: "tx-1", SourceAccount: "a", TargetAccount: "a",
				AmountMinor: 100, Currency: "USD",
			},
		},
		{
			name: "unsupported currency",
			transfer: Transfer{
				TransactionID: "tx-1", SourceAccount: "a", TargetAccount: "b",
				AmountMinor: 100, Currency: "EUR",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeTransferStore{}
			service := NewService(store, "us-east-1", true)

			_, err := service.CreateTransfer(context.Background(), tt.transfer)
			if !errors.Is(err, ErrInvalidTransfer) {
				t.Fatalf("error = %v, want ErrInvalidTransfer", err)
			}
			if store.calls != 0 {
				t.Fatalf("store calls = %d, want 0", store.calls)
			}
		})
	}
}

func TestServicePropagatesStoreError(t *testing.T) {
	storeErr := errors.New("store unavailable")
	service := NewService(&fakeTransferStore{err: storeErr}, "us-east-1", true)

	_, err := service.CreateTransfer(context.Background(), Transfer{
		TransactionID: "tx-1", SourceAccount: "a", TargetAccount: "b",
		AmountMinor: 100, Currency: "USD",
	})
	if !errors.Is(err, storeErr) {
		t.Fatalf("error = %v, want wrapped store error", err)
	}
}

func TestServiceDoesNotWriteWhenRegionIsStandby(t *testing.T) {
	store := &fakeTransferStore{}
	service := NewService(store, "us-west-2", false)

	_, err := service.CreateTransfer(context.Background(), Transfer{
		TransactionID: "tx-1", SourceAccount: "a", TargetAccount: "b",
		AmountMinor: 100, Currency: "USD",
	})
	if !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("error = %v, want ErrWritesDisabled", err)
	}
	if store.calls != 0 {
		t.Fatalf("store calls = %d, want 0", store.calls)
	}
}
