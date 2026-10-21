// Package pager shows long output one screen at a time, like more(1).
package pager

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"sqlcli/internal/format"
)

const prompt = "-- More (%d%%) -- Enter/Space: next page, j/Down: next line, Esc/q/Ctrl+Q: quit"

// Page writes text to w. If it is taller than height rows it is shown one
// page at a time, reading keys from in. width is used to account for lines
// the terminal wraps.
func Page(w io.Writer, in *os.File, text string, width, height int) error {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	rows := func(l string) int {
		dw := format.DisplayWidth(l)
		if width <= 0 || dw <= width {
			return 1
		}
		return (dw + width - 1) / width
	}
	total := 0
	for _, l := range lines {
		total += rows(l)
	}
	if height < 2 || total <= height {
		_, err := io.WriteString(w, text)
		return err
	}

	pos, shown := 0, 0
	show := func(budget int) {
		used := 0
		for pos < len(lines) {
			r := rows(lines[pos])
			if used > 0 && used+r > budget {
				break
			}
			fmt.Fprintln(w, lines[pos])
			used += r
			shown += r
			pos++
		}
	}
	show(height - 1)
	for pos < len(lines) {
		p := fmt.Sprintf(prompt, shown*100/total)
		if width > 0 && len(p) >= width {
			p = p[:width-1]
		}
		fmt.Fprint(w, p)
		key, err := readKey(in)
		fmt.Fprint(w, "\r"+strings.Repeat(" ", len(p))+"\r")
		if err != nil {
			return err
		}
		switch key {
		case keyQuit:
			return nil
		case keyLine:
			show(1)
		case keyPage:
			show(height - 1)
		}
	}
	return nil
}

type key int

const (
	keyNone key = iota
	keyPage
	keyLine
	keyQuit
)

func readKey(in *os.File) (key, error) {
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return keyQuit, err
	}
	defer term.Restore(int(in.Fd()), state)
	buf := make([]byte, 16)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return keyQuit, err
		}
		b := buf[:n]
		switch {
		case n == 1 && (b[0] == 0x1b || b[0] == 0x11 || b[0] == 0x03 || b[0] == 'q' || b[0] == 'Q'):
			return keyQuit, nil
		case n == 1 && (b[0] == '\r' || b[0] == '\n' || b[0] == ' '):
			return keyPage, nil
		case n == 1 && b[0] == 'j', string(b) == "\x1b[B", string(b) == "\x1bOB":
			return keyLine, nil
		case string(b) == "\x1b[6~": // PageDown
			return keyPage, nil
		}
	}
}
