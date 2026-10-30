package ccerr_test

import (
	"fmt"
	"testing"

	"github.com/muhammadtalha198/fabricsdk/ccerr"
)

func TestNewRendersParsableMessage(t *testing.T) {
	err := ccerr.New(ccerr.NotFound, "challan %s does not exist", "CH-1")
	if err.Error() != "NOT_FOUND: challan CH-1 does not exist" {
		t.Fatalf("Error() = %q", err.Error())
	}
	code, text, ok := ccerr.Parse(err.Error())
	if !ok || code != ccerr.NotFound || text != "challan CH-1 does not exist" {
		t.Fatalf("Parse = %q %q %v", code, text, ok)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		code ccerr.Code
		text string
		ok   bool
	}{
		{"CONFLICT: version 2 != 3", ccerr.Conflict, "version 2 != 3", true},
		{"chaincode response 409, ALREADY_EXISTS: CH-1", ccerr.AlreadyExists, "CH-1", true},
		{"  UNAUTHORIZED:missing role", ccerr.Unauthorized, "missing role", true},
		{"the asset a1 does not exist", "", "the asset a1 does not exist", false},
		{"ERROR: something", "", "ERROR: something", false},
		{"http://example: not a code", "", "http://example: not a code", false},
	}
	for _, tt := range tests {
		code, text, ok := ccerr.Parse(tt.in)
		if code != tt.code || text != tt.text || ok != tt.ok {
			t.Errorf("Parse(%q) = %q %q %v", tt.in, code, text, ok)
		}
	}
}

func TestIsSeesThroughWrapping(t *testing.T) {
	err := fmt.Errorf("issue challan: %w", ccerr.New(ccerr.FailedPrecondition, "already paid"))
	if !ccerr.Is(err, ccerr.FailedPrecondition) || ccerr.Is(err, ccerr.NotFound) {
		t.Fatal("Is did not match the wrapped code")
	}
}
