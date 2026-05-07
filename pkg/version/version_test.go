package version

import "testing"

func TestFull(t *testing.T) {
	got := Full()
	want := "dev (unknown)"
	if got != want {
		t.Fatalf("Full() = %q, want %q", got, want)
	}
}
