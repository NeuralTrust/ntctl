package prompt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestScripted(t *testing.T) {
	var out bytes.Buffer
	p := Scripted(strings.NewReader("\ncustom\n\n7\n2\nhunter2\ny\n"), &out)

	if v, _ := p.Text("Domain", "a.example.com"); v != "a.example.com" {
		t.Errorf("Enter should keep the default, got %q", v)
	}
	if v, _ := p.Text("Domain", "a.example.com"); v != "custom" {
		t.Errorf("got %q", v)
	}
	// Enter on a choice keeps the default; an out-of-range number is asked again.
	if i, _ := p.Choose("Pick", []string{"one", "two"}, 0); i != 0 {
		t.Errorf("default choice = %d", i)
	}
	if i, _ := p.Choose("Pick", []string{"one", "two"}, 0); i != 1 {
		t.Errorf("choice = %d, want 1 after re-asking", i)
	}
	if v, _ := p.Secret("Password"); v != "hunter2" {
		t.Errorf("secret = %q", v)
	}
	if err := p.Confirm("Continue?"); err != nil {
		t.Errorf("confirm: %v", err)
	}
	if !strings.Contains(out.String(), "Enter a number from 1 to 2") {
		t.Errorf("no re-ask message:\n%s", out.String())
	}
}

func TestNotInteractive(t *testing.T) {
	p := New(context.Background(), strings.NewReader("ignored\n"), &bytes.Buffer{}, false)
	if v, err := p.Text("Domain", "d"); v != "d" || err != nil {
		t.Errorf("default not taken: %q %v", v, err)
	}
	if _, err := p.Text("Domain", ""); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("required answer without a terminal: %v", err)
	}
	if i, _ := p.Choose("Pick", []string{"a", "b"}, 1); i != 1 {
		t.Errorf("choice = %d", i)
	}
	if _, err := p.Secret("Token"); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("secret without a terminal: %v", err)
	}
	if err := p.Confirm("Continue?"); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("confirm without a terminal must fail, got %v", err)
	}
}

func TestYesNo(t *testing.T) {
	var out bytes.Buffer
	p := Scripted(strings.NewReader("\nY\nmaybe\nno\n"), &out)
	for i, want := range []bool{false, true, false} {
		if got, err := p.YesNo("Turn it on?", false); err != nil || got != want {
			t.Errorf("answer %d = %v, %v; want %v", i, got, err, want)
		}
	}
	if !strings.Contains(out.String(), "[y/N]") || !strings.Contains(out.String(), "Answer y or n.") {
		t.Errorf("unexpected prompt output:\n%s", out.String())
	}
	if got, err := New(context.Background(), strings.NewReader("y\n"), &bytes.Buffer{}, false).YesNo("Turn it on?", false); got || err != nil {
		t.Errorf("without a terminal the default must be taken, got %v, %v", got, err)
	}
}

// blockingReader never returns, like a terminal nobody types in.
type blockingReader struct{ unblock chan struct{} }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.unblock
	return 0, io.EOF
}

// TestInterruptEndsAQuestion is Ctrl+C at a prompt: the command's context is
// cancelled while the read is blocked on a terminal, and every kind of
// question returns ErrInterrupted at once instead of waiting for Enter.
func TestInterruptEndsAQuestion(t *testing.T) {
	asks := map[string]func(*Prompter) error{
		"text":    func(p *Prompter) error { _, err := p.Text("Domain", ""); return err },
		"choose":  func(p *Prompter) error { _, err := p.Choose("Platform", []string{"aws", "gcp"}, 0); return err },
		"yesno":   func(p *Prompter) error { _, err := p.YesNo("Turn it on?", false); return err },
		"confirm": func(p *Prompter) error { return p.Confirm("Continue?") },
		"secret":  func(p *Prompter) error { _, err := p.Secret("Token"); return err },
	}
	for name, ask := range asks {
		t.Run(name, func(t *testing.T) {
			stdin := blockingReader{unblock: make(chan struct{})}
			defer close(stdin.unblock)
			ctx, cancel := context.WithCancel(context.Background())
			p := Scripted(stdin, io.Discard)
			p.ctx = ctx

			done := make(chan error, 1)
			go func() { done <- ask(p) }()
			time.Sleep(20 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, ErrInterrupted) {
					t.Fatalf("err = %v, want ErrInterrupted", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the question kept waiting for input after the interrupt")
			}
		})
	}
}

func TestInterruptedBeforeAsking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := Scripted(strings.NewReader("an answer\n"), io.Discard)
	p.ctx = ctx
	if _, err := p.Text("Domain", ""); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted: an interrupt already received must not be answered", err)
	}
}
