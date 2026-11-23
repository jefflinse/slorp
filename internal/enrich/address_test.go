package enrich

import (
	"context"
	"strings"
	"testing"
)

func TestServerURL(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  http://localhost:1234  ", "http://localhost:1234", true},
		{"http://localhost:11434/v1", "http://localhost:11434", true},
		{"http://localhost:11434/v1/", "http://localhost:11434", true},
		{"localhost:8080", "http://localhost:8080", true},
		{"https://models.example/openai/v1?key=1#top", "https://models.example/openai", true},
		{"http://127.0.0.1:1234/", "http://127.0.0.1:1234", true},
		{"ftp://localhost:21", "", false},
		{"http://", "", false},
		{"http://user:secret@localhost:1234", "", false},
		{"http://local host", "", false},
	}
	for _, tt := range tests {
		if got, ok := ServerURL(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("ServerURL(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestStartupAddressesAreCheckedAsSettingsAre(t *testing.T) {
	for in, want := range map[string]string{"": "", "localhost:1234/v1/": "http://localhost:1234", DefaultURL: DefaultURL} {
		got, err := Startup{URL: in, URLGiven: true}.Checked()
		if err != nil || got.URL != want {
			t.Errorf("starting with %q = %q, %v; want %q", in, got.URL, err, want)
		}
	}
	for _, in := range []string{"ftp://localhost:21", "http://local host", "http://user:secret@localhost"} {
		if _, err := (Startup{URL: in, URLGiven: true}).Checked(); err == nil || !strings.Contains(err.Error(), in) {
			t.Errorf("starting with %q = %v, want an error naming it", in, err)
		}
	}
}

// A server address kept before Settings checked it fails each request rather
// than the dashboard.
func TestARequestToAnUnusableAddressFailsWithoutPanicking(t *testing.T) {
	e := &Enricher{}
	for _, base := range []string{"http://local host", "http://[::1", "\x7f"} {
		if err := e.get(context.Background(), base, "/v1/models", &struct{}{}); err == nil {
			t.Errorf("GET from %q succeeded", base)
		}
		if _, err := e.chat(context.Background(), base, map[string]any{"model": "m"}); err == nil {
			t.Errorf("chat with %q succeeded", base)
		}
	}
}
