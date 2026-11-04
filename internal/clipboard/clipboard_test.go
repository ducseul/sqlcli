package clipboard

import (
	"errors"
	"os"
	"testing"
)

// TestRoundTrip touches the real clipboard, so it only runs on request.
func TestRoundTrip(t *testing.T) {
	if os.Getenv("SQLCLI_TEST_CLIPBOARD") == "" {
		t.Skip("SQLCLI_TEST_CLIPBOARD not set")
	}
	want := "select 'héllo', 42\nfrom dual"
	if err := Write(want); err != nil {
		if errors.Is(err, ErrUnavailable) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	got, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
