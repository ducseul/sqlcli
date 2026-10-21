package pager

import (
	"os"
	"strings"
	"testing"
)

func TestShortOutputNotPaged(t *testing.T) {
	var b strings.Builder
	text := "a\nb\nc\n"
	if err := Page(&b, os.Stdin, text, 80, 24); err != nil {
		t.Fatal(err)
	}
	if b.String() != text {
		t.Fatalf("got %q", b.String())
	}
}
