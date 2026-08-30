package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// TestIsCleanShutdown covers how `bees mcp serve` ends. claude closes the
// server's stdin when it is done with it, which the SDK reports as a
// jsonrpc2 "server is closing" error; exiting nonzero there would make every
// session record its bees server as having crashed.
func TestIsCleanShutdown(t *testing.T) {
	closing := &jsonrpc.Error{Code: codeServerClosing, Message: "server is closing"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"eof", io.EOF, true},
		{"cancelled", context.Canceled, true},
		{"closing", closing, true},
		{"closing wrapping the read error", fmt.Errorf("%w: %v", closing, io.EOF), true},
		{"wrapped further", fmt.Errorf("run: %w", closing), true},
		{"another wire error", &jsonrpc.Error{Code: jsonrpc.CodeInternalError}, false},
		{"a real failure", errors.New("broken pipe"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCleanShutdown(tc.err); got != tc.want {
				t.Fatalf("isCleanShutdown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
