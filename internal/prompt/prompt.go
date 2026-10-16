// Package prompt asks install questions on a terminal: free text with a
// default, a numbered choice, yes or no, hidden input, and confirmation.
package prompt

import (
	"bufio"
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

// Prompter reads answers from a terminal.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
	fd  int
	tty bool
	// Interactive is false when stdin is not a terminal or the operator asked
	// for no questions; every question then takes its default.
	Interactive bool
}

// New returns a prompter on stdin/out. Questions are asked only when stdin is
// a terminal and ask is true.
func New(stdin io.Reader, out io.Writer, ask bool) *Prompter {
	p := &Prompter{in: bufio.NewReader(stdin), out: out}
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.fd, p.tty = int(f.Fd()), true
	}
	p.Interactive = p.tty && ask
	return p
}

// Scripted returns a prompter that answers from r as if typed, for tests.
func Scripted(r io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: bufio.NewReader(r), out: out, Interactive: true}
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
			b, err := term.ReadPassword(p.fd)
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
	s, err := p.in.ReadString('\n')
	// A last line without a newline still counts as an answer.
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		return "", err
	}
	return strings.TrimSpace(s), nil
}
