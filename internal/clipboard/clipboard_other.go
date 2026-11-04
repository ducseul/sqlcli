//go:build !windows

package clipboard

import (
	"os"
	"os/exec"
	"strings"
)

type tool struct{ copy, paste []string }

func tools() []tool {
	var ts []tool
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		ts = append(ts, tool{[]string{"wl-copy"}, []string{"wl-paste", "--no-newline"}})
	}
	ts = append(ts,
		tool{[]string{"pbcopy"}, []string{"pbpaste"}},
		tool{[]string{"xclip", "-selection", "clipboard"}, []string{"xclip", "-selection", "clipboard", "-o"}},
		tool{[]string{"xsel", "--clipboard", "--input"}, []string{"xsel", "--clipboard", "--output"}},
	)
	return ts
}

func find(pick func(tool) []string) []string {
	for _, t := range tools() {
		args := pick(t)
		if _, err := exec.LookPath(args[0]); err == nil {
			// X11 tools need a display; without one they hang or fail.
			if (args[0] == "xclip" || args[0] == "xsel") && os.Getenv("DISPLAY") == "" {
				continue
			}
			return args
		}
	}
	return nil
}

func write(text string) error {
	args := find(func(t tool) []string { return t.copy })
	if args == nil {
		return ErrUnavailable
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func read() (string, error) {
	args := find(func(t tool) []string { return t.paste })
	if args == nil {
		return "", ErrUnavailable
	}
	out, err := exec.Command(args[0], args[1:]...).Output()
	return string(out), err
}
