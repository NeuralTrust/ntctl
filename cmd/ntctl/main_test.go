package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/prompt"
)

func TestExitCodeOfAnInterrupt(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		code int
		says string
	}{
		{"at a question, wrapped with a hint", cancelled, fmt.Errorf("%w; set NT_TOKEN", prompt.ErrInterrupted), 130, "Interrupted."},
		{"during the run", cancelled, context.Canceled, 130, "Error: context canceled\nInterrupted."},
		{"not interrupted", context.Background(), errors.New("boom"), 1, "Error: boom"},
		{"preflight", context.Background(), errPreflight, 3, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := exitCode(c.ctx, c.err, &stderr); got != c.code {
				t.Errorf("exit code = %d, want %d", got, c.code)
			}
			if !strings.Contains(stderr.String(), c.says) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), c.says)
			}
			if c.code == 130 && c.err != context.Canceled && strings.Contains(stderr.String(), "NT_TOKEN") {
				t.Errorf("an interrupt at a question should not print the question's hint: %q", stderr.String())
			}
		})
	}
}
