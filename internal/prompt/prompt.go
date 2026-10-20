// Package prompt asks install questions on a terminal: free text with a
// default, a numbered choice, yes or no, hidden input, and confirmation.
package prompt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrNoTerminal is returned when an answer is needed and nobody can type it.
var ErrNoTerminal = errors.New("input needed but no terminal is attached")

// ErrInterrupted is returned when the operator interrupts a question (Ctrl+C).
var ErrInterrupted = errors.New("interrupted")

// Prompter reads answers from a terminal.
type Prompter struct {
	// ctx ends a question waiting for input: the command's context, cancelled
	// by Ctrl+C. Reading stdin cannot be interrupted, so each read runs on its
	// own goroutine and the question returns ErrInterrupted without it.
	ctx context.Context
	in  *bufio.Reader
	out io.Writer
	fd  int
	tty bool
	// Interactive is false when stdin is not a terminal or the operator asked
	// for no questions; every question then takes its default.
	Interactive bool
}

// New returns a prompter on stdin/out. Questions are asked only when stdin is
// a terminal and ask is true. Cancelling ctx ends a question waiting for
// input with ErrInterrupted.
func New(ctx context.Context, stdin io.Reader, out io.Writer, ask bool) *Prompter {
	p := &Prompter{ctx: ctx, in: bufio.NewReader(stdin), out: out}
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.fd, p.tty = int(f.Fd()), true
	}
	p.Interactive = p.tty && ask
	return p
}

// Scripted returns a prompter that answers from r as if typed, for tests.
func Scripted(r io.Reader, out io.Writer) *Prompter {
	return &Prompter{ctx: context.Background(), in: bufio.NewReader(r), out: out, Interactive: true}
}

// Text asks for a line. Enter keeps def; with no def an answer is required.
func (p *Prompter) Text(question, def string) (string, error) {
	if !p.Interactive {
		if def == "" {
			return "", fmt.Errorf("%w (%s)", ErrNoTerminal, question)
		}
		return def, nil
	}
	for {
		if def != "" {
			fmt.Fprintf(p.out, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(p.out, "%s: ", question)
		}
		line, err := p.line()
		if err != nil {
			return "", err
		}
		if line == "" {
			line = def
		}
		if line != "" {
			return line, nil
		}
		fmt.Fprintln(p.out, "  An answer is required.")
	}
}

// Secret asks for a line without echoing it. There is no default.
func (p *Prompter) Secret(question string) (string, error) {
	if !p.Interactive {
		return "", fmt.Errorf("%w (%s)", ErrNoTerminal, question)
	}
	for {
		fmt.Fprintf(p.out, "%s (input hidden): ", question)
		var line string
		if p.tty {
			b, err := p.readPassword()
			fmt.Fprintln(p.out)
			if err != nil {
				return "", err
			}
			line = strings.TrimSpace(string(b))
		} else {
			s, err := p.line()
			if err != nil {
				return "", err
			}
			line = s
		}
		if line != "" {
			return line, nil
		}
		fmt.Fprintln(p.out, "  An answer is required.")
	}
}

// Choose asks the operator to pick one option by number. def is the index
// used on Enter, and when not interactive.
func (p *Prompter) Choose(question string, options []string, def int) (int, error) {
	if !p.Interactive {
		return def, nil
	}
	fmt.Fprintln(p.out, question)
	for i, o := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, o)
	}
	for {
		fmt.Fprintf(p.out, "Choose [%d]: ", def+1)
		line, err := p.line()
		if err != nil {
			return 0, err
		}
		if line == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(p.out, "  Enter a number from 1 to %d.\n", len(options))
	}
}

// YesNo asks a yes/no question. Enter keeps def, which is also the answer
// when not interactive.
func (p *Prompter) YesNo(question string, def bool) (bool, error) {
	if !p.Interactive {
		return def, nil
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(p.out, "%s [%s]: ", question, hint)
		line, err := p.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(line) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.out, "  Answer y or n.")
	}
}

// Confirm asks a yes/no question that defaults to no. Without a terminal it
// fails, so a change is never confirmed by accident; callers offer --yes.
func (p *Prompter) Confirm(question string) error {
	if !p.Interactive {
		return fmt.Errorf("%w; pass --yes to continue without confirmation", ErrNoTerminal)
	}
	fmt.Fprintf(p.out, "%s [y/N]: ", question)
	line, err := p.line()
	if err != nil {
		return err
	}
	if a := strings.ToLower(line); a != "y" && a != "yes" {
		return errors.New("aborted")
	}
	return nil
}

func (p *Prompter) line() (string, error) {
	s, err := await(p.ctx, func() (string, error) { return p.in.ReadString('\n') })
	if errors.Is(err, ErrInterrupted) {
		return "", err
	}
	// A last line without a newline still counts as an answer.
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// readPassword reads a line with echo off. ReadPassword turns echo back on
// only when it returns, which it does not when interrupted, so the terminal
// state is restored here first: the shell would otherwise be left without echo.
func (p *Prompter) readPassword() ([]byte, error) {
	state, stateErr := term.GetState(p.fd)
	b, err := await(p.ctx, func() ([]byte, error) { return term.ReadPassword(p.fd) })
	if errors.Is(err, ErrInterrupted) && stateErr == nil {
		_ = term.Restore(p.fd, state) // best effort: the run is ending anyway
	}
	return b, err
}

// await runs a blocking read on its own goroutine and returns its result, or
// ErrInterrupted once ctx is cancelled. An interrupted read stays blocked on
// stdin; the command is ending, so it is left to the process exit.
func await[T any](ctx context.Context, read func() (T, error)) (T, error) {
	var zero T
	if ctx == nil {
		return read()
	}
	if ctx.Err() != nil {
		return zero, ErrInterrupted
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := read()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ErrInterrupted
	}
}
