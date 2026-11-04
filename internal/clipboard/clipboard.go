// Package clipboard reads and writes the system clipboard as text.
//
// Windows uses the Win32 clipboard API. Other systems use the first helper
// found on PATH: pbcopy/pbpaste (macOS), wl-copy/wl-paste (Wayland), xclip or
// xsel (X11). ErrUnavailable is returned when there is none (e.g. over SSH);
// callers can then fall back to the terminal (OSC 52).
package clipboard

import "errors"

// ErrUnavailable means no system clipboard could be reached.
var ErrUnavailable = errors.New("system clipboard not available")

// Write puts text on the system clipboard.
func Write(text string) error { return write(text) }

// Read returns the text on the system clipboard.
func Read() (string, error) { return read() }
