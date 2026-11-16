package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoDBAPI interface {
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

type DynamoDBStore struct {
	client    DynamoDBAPI
	tableName string
}

func NewDynamoDBStore(client DynamoDBAPI, tableName string) *DynamoDBStore {
	return &DynamoDBStore{
		client:    client,
		tableName: tableName,
	}
}

func (s *DynamoDBStore) CreateTransfer(ctx context.Context, transfer Transfer) error {
	amount := strconv.FormatInt(transfer.AmountMinor, 10)
	createdAt := transfer.CreatedAt.UTC().Format(time.RFC3339Nano)
	commonNames := map[string]string{
		"#balance":    "balance_minor",
		"#currency":   "currency",
		"#updated_at": "updated_at",
	}
	commonValues := map[string]types.AttributeValue{
		":amount":   &types.AttributeValueMemberN{Value: amount},
		":currency": &types.AttributeValueMemberS{Value: transfer.Currency},
		":updated":  &types.AttributeValueMemberS{Value: createdAt},
	}

	input := &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				Update: &types.Update{
					TableName: aws.String(s.tableName),
					Key:       accountKey(transfer.SourceAccount),
					UpdateExpression: aws.String(
						"SET #balance = #balance - :amount, #updated_at = :updated",
					),
					ConditionExpression: aws.String(
						"attribute_exists(#balance) AND #balance >= :amount AND #currency = :currency",
					),
					ExpressionAttributeNames:  commonNames,
					ExpressionAttributeValues: commonValues,
				},
			},
			{
				Update: &types.Update{
					TableName: aws.String(s.tableName),
					Key:       accountKey(transfer.TargetAccount),
					UpdateExpression: aws.String(
						"SET #balance = #balance + :amount, #updated_at = :updated",
					),
					ConditionExpression: aws.String(
						"attribute_exists(#balance) AND #currency = :currency",
					),
					ExpressionAttributeNames:  commonNames,
					ExpressionAttributeValues: commonValues,
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String(s.tableName),
					Item:      transferItem(transfer),
					ConditionExpression: aws.String(
						"attribute_not_exists(PK)",
					),
				},
			},
		},
	}

	if _, err := s.client.TransactWriteItems(ctx, input); err != nil {
		return classifyTransactionError(err)
	}
	return nil
}

func (s *DynamoDBStore) Check(ctx context.Context) error {
	_, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		return fmt.Errorf("describe DynamoDB table: %w", err)
	}
	return nil
}

func classifyTransactionError(err error) error {
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return fmt.Errorf("transact DynamoDB items: %w", err)
	}

	for index, reason := range canceled.CancellationReasons {
		if aws.ToString(reason.Code) != "ConditionalCheckFailed" {
			continue
		}

		switch index {
		case 0:
			return fmt.Errorf("%w: %w", ErrInsufficientBalance, err)
		case 1:
			return fmt.Errorf("%w: %w", ErrAccountConditionFailed, err)
		case 2:
			return fmt.Errorf("%w: %w", ErrDuplicateTransfer, err)
		}
	}

	return fmt.Errorf("DynamoDB transaction canceled: %w", err)
}

func accountKey(accountID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "ACCOUNT#" + accountID},
		"SK": &types.AttributeValueMemberS{Value: "BALANCE"},
	}
}

func transferItem(transfer Transfer) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK":              &types.AttributeValueMemberS{Value: "TRANSFER#" + transfer.TransactionID},
		"SK":              &types.AttributeValueMemberS{Value: "META"},
		"TransactionID":   &types.AttributeValueMemberS{Value: transfer.TransactionID},
		"SourceAccount":   &types.AttributeValueMemberS{Value: transfer.SourceAccount},
		"TargetAccount":   &types.AttributeValueMemberS{Value: transfer.TargetAccount},
		"AmountMinor":     &types.AttributeValueMemberN{Value: strconv.FormatInt(transfer.AmountMinor, 10)},
		"Currency":        &types.AttributeValueMemberS{Value: transfer.Currency},
		"Status":          &types.AttributeValueMemberS{Value: transfer.Status},
		"ProcessedRegion": &types.AttributeValueMemberS{Value: transfer.ProcessedRegion},
		"CreatedAt":       &types.AttributeValueMemberS{Value: transfer.CreatedAt.UTC().Format(time.RFC3339Nano)},
	}
}
