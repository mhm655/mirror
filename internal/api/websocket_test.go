package api

import "testing"

func TestHeaderContainsToken(t *testing.T) {
	cases := []struct {
		header, token string
		want          bool
	}{
		{"mirror.v1", "mirror.v1", true},
		{"mirror.v1, chat", "chat", true},
		{"  mirror.v1  ,  chat  ", "MIRROR.V1", true}, // case-insensitive, trims space
		{"chat", "mirror.v1", false},
		{"", "mirror.v1", false},
	}
	for _, c := range cases {
		if got := headerContainsToken(c.header, c.token); got != c.want {
			t.Errorf("headerContainsToken(%q, %q) = %v, want %v", c.header, c.token, got, c.want)
		}
	}
}
