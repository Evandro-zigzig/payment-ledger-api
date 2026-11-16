package ledger

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidTransfer        = errors.New("invalid transfer")
	ErrDuplicateTransfer      = errors.New("duplicate transfer")
	ErrInsufficientBalance    = errors.New("insufficient source balance or currency mismatch")
	ErrAccountConditionFailed = errors.New("destination account missing or currency mismatch")
	ErrWritesDisabled         = errors.New("writes are disabled in this region")
)

type Transfer struct {
	TransactionID   string    `json:"transaction_id"`
	SourceAccount   string    `json:"source_account"`
	TargetAccount   string    `json:"target_account"`
	AmountMinor     int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	Status          string    `json:"status"`
	ProcessedRegion string    `json:"processed_region"`
	CreatedAt       time.Time `json:"created_at"`
}

type TransferStore interface {
	CreateTransfer(context.Context, Transfer) error
	Check(context.Context) error
}
