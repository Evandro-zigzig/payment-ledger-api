package main

import (
	"strings"
	"testing"
)

func TestRunRequiresExplicitConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		table       string
		writes      string
		wantMessage string
	}{
		{
			name:        "table is required",
			writes:      "false",
			wantMessage: "DYNAMODB_TABLE is required",
		},
		{
			name:        "write mode must be explicit",
			table:       "test-ledger",
			wantMessage: "WRITES_ENABLED must be explicitly set to true or false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DYNAMODB_TABLE", tt.table)
			t.Setenv("WRITES_ENABLED", tt.writes)

			err := run()
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("run() error = %v, want it to contain %q", err, tt.wantMessage)
			}
		})
	}
}
