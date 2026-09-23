package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const maxTerminalTextBytes = 2000

func terminalText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
func confirm(in *bufio.Reader, out io.Writer, prompt string) error {
	fmt.Fprint(out, prompt)
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(line) != "yes" {
		return fmt.Errorf("confirmation declined")
	}
	return nil
}

// Preserve readable multiline responses/diffs while removing terminal controls.
func terminalLines(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
