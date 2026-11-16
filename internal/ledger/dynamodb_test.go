package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDynamoDB struct {
	input       *dynamodb.TransactWriteItemsInput
	transactErr error
	describeErr error
}

func (f *fakeDynamoDB) TransactWriteItems(_ context.Context, input *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.input = input
	return &dynamodb.TransactWriteItemsOutput{}, f.transactErr
}

func (f *fakeDynamoDB) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, f.describeErr
}

func TestDynamoDBStoreCreatesAtomicTransfer(t *testing.T) {
	client := &fakeDynamoDB{}
	store := NewDynamoDBStore(client, "ledger")
	transfer := Transfer{
		TransactionID:   "tx-1",
		SourceAccount:   "source",
		TargetAccount:   "target",
		AmountMinor:     1250,
		Currency:        "USD",
		Status:          "SETTLED",
		ProcessedRegion: "us-east-1",
		CreatedAt:       time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
	}

	if err := store.CreateTransfer(context.Background(), transfer); err != nil {
		t.Fatalf("CreateTransfer() error = %v", err)
	}
	if client.input == nil {
		t.Fatal("TransactWriteItems was not called")
	}
	if got := len(client.input.TransactItems); got != 3 {
		t.Fatalf("transaction actions = %d, want 3", got)
	}

	sourceUpdate := client.input.TransactItems[0].Update
	if aws.ToString(sourceUpdate.TableName) != "ledger" {
		t.Fatalf("source table = %q, want ledger", aws.ToString(sourceUpdate.TableName))
	}
	if got := sourceUpdate.Key["PK"].(*types.AttributeValueMemberS).Value; got != "ACCOUNT#source" {
		t.Fatalf("source key = %q, want ACCOUNT#source", got)
	}
	if got := aws.ToString(sourceUpdate.ConditionExpression); got != "attribute_exists(#balance) AND #balance >= :amount AND #currency = :currency" {
		t.Fatalf("source condition = %q", got)
	}

	targetUpdate := client.input.TransactItems[1].Update
	if got := aws.ToString(targetUpdate.UpdateExpression); got != "SET #balance = #balance + :amount, #updated_at = :updated" {
		t.Fatalf("target update = %q", got)
	}

	recordPut := client.input.TransactItems[2].Put
	if got := aws.ToString(recordPut.ConditionExpression); got != "attribute_not_exists(PK)" {
		t.Fatalf("idempotency condition = %q", got)
	}
	if got := recordPut.Item["AmountMinor"].(*types.AttributeValueMemberN).Value; got != "1250" {
		t.Fatalf("stored amount = %q, want integer minor units 1250", got)
	}
}

func TestDynamoDBStoreClassifiesTransactionConditions(t *testing.T) {
	tests := []struct {
		name    string
		reasons []types.CancellationReason
		wantErr error
	}{
		{
			name: "insufficient source balance",
			reasons: []types.CancellationReason{
				{Code: aws.String("ConditionalCheckFailed")},
				{Code: aws.String("None")},
				{Code: aws.String("None")},
			},
			wantErr: ErrInsufficientBalance,
		},
		{
			name: "destination account condition failed",
			reasons: []types.CancellationReason{
				{Code: aws.String("None")},
				{Code: aws.String("ConditionalCheckFailed")},
				{Code: aws.String("None")},
			},
			wantErr: ErrAccountConditionFailed,
		},
		{
			name: "duplicate transfer",
			reasons: []types.CancellationReason{
				{Code: aws.String("None")},
				{Code: aws.String("None")},
				{Code: aws.String("ConditionalCheckFailed")},
			},
			wantErr: ErrDuplicateTransfer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyTransactionError(&types.TransactionCanceledException{
				CancellationReasons: tt.reasons,
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDynamoDBStoreCheckPropagatesError(t *testing.T) {
	wantErr := errors.New("table unavailable")
	store := NewDynamoDBStore(&fakeDynamoDB{describeErr: wantErr}, "ledger")

	err := store.Check(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Check() error = %v, want wrapped error", err)
	}
}
