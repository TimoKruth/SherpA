package registryurl

import "testing"

func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{" HTTPS://Registry.Example:443/prefix/ ", "https://registry.example/prefix"},
		{"http://REGISTRY.example:80", "http://registry.example"},
		{"http://[2001:db8::1]:80/a", "http://[2001:db8::1]/a"},
	} {
		got, err := Normalize(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("Normalize(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"", "registry.example", "ftp://registry.example", "https://u:p@registry.example", "https://registry.example?q=x", "https://registry.example#x"} {
		if got, err := Normalize(raw); err == nil {
			t.Fatalf("Normalize(%q) = %q, want error", raw, got)
		}
	}
}
