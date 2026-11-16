package ledger

import (
	"context"
	"strings"
	"time"
)

type Service struct {
	store         TransferStore
	region        string
	writesEnabled bool
	now           func() time.Time
}

func NewService(store TransferStore, region string, writesEnabled bool) *Service {
	return &Service{
		store:         store,
		region:        region,
		writesEnabled: writesEnabled,
		now:           time.Now,
	}
}

func (s *Service) CreateTransfer(ctx context.Context, transfer Transfer) (Transfer, error) {
	transfer.TransactionID = strings.TrimSpace(transfer.TransactionID)
	transfer.SourceAccount = strings.TrimSpace(transfer.SourceAccount)
	transfer.TargetAccount = strings.TrimSpace(transfer.TargetAccount)
	transfer.Currency = strings.TrimSpace(transfer.Currency)

	if transfer.TransactionID == "" ||
		transfer.SourceAccount == "" ||
		transfer.TargetAccount == "" ||
		transfer.SourceAccount == transfer.TargetAccount ||
		transfer.AmountMinor <= 0 ||
		transfer.Currency != "USD" {
		return Transfer{}, ErrInvalidTransfer
	}
	if !s.writesEnabled {
		return Transfer{}, ErrWritesDisabled
	}

	transfer.Status = "SETTLED"
	transfer.ProcessedRegion = s.region
	transfer.CreatedAt = s.now().UTC()

	if err := s.store.CreateTransfer(ctx, transfer); err != nil {
		return Transfer{}, err
	}

	return transfer, nil
}

func (s *Service) Check(ctx context.Context) error {
	if !s.writesEnabled {
		return ErrWritesDisabled
	}
	return s.store.Check(ctx)
}
