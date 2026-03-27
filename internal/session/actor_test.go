package session

import (
	"testing"

	"github.com/jialuohu/curlycatclaw/config"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"ab", 1, "a..."},
	}
	for _, tc := range cases {
		got := truncate(tc.input, tc.max)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
		}
	}
}

func TestRequiresConfirmation(t *testing.T) {
	a := &Actor{
		cfg: &config.Config{
			ConfirmTools: []string{"cancel_reminder", "filesystem__delete"},
		},
	}

	cases := []struct {
		tool string
		want bool
	}{
		{"cancel_reminder", true},
		{"filesystem__delete_file", true},
		{"web_search", false},
		{"save_note", false},
		{"", false},
	}
	for _, tc := range cases {
		got := a.requiresConfirmation(tc.tool)
		if got != tc.want {
			t.Errorf("requiresConfirmation(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

func TestRequiresConfirmation_EmptyList(t *testing.T) {
	a := &Actor{cfg: &config.Config{}}

	if a.requiresConfirmation("anything") {
		t.Error("empty ConfirmTools list should never require confirmation")
	}
}
