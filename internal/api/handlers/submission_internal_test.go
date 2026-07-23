package handlers

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseTokenList(t *testing.T) {
	valid1 := uuid.NewString()
	valid2 := uuid.NewString()

	tests := []struct {
		name      string
		raw       string
		wantLen   int
		wantErr   string // substring expected in the error, empty = no error
	}{
		{name: "single token", raw: valid1, wantLen: 1},
		{name: "two tokens", raw: valid1 + "," + valid2, wantLen: 2},
		{name: "trims whitespace", raw: " " + valid1 + " , " + valid2 + " ", wantLen: 2},
		{name: "skips empty segments", raw: valid1 + ",," + valid2 + ",", wantLen: 2},
		{name: "empty input", raw: "", wantErr: "at least one token"},
		{name: "only commas", raw: ",, ,", wantErr: "at least one token"},
		{name: "malformed uuid", raw: valid1 + ",not-a-uuid", wantErr: "invalid token format"},
		{name: "too many tokens", raw: strings.TrimRight(strings.Repeat(uuid.NewString()+",", 21), ","), wantErr: "maximum 20"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokens, err := parseTokenList(tc.raw)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(tokens) != tc.wantLen {
				t.Fatalf("expected %d tokens, got %d (%v)", tc.wantLen, len(tokens), tokens)
			}
		})
	}
}
