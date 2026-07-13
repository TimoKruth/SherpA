package web

import (
	"os"
	"strings"
	"testing"
)

func TestStaticVisualAssetsKeepSecurityAndProgressiveEnhancementContracts(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	javascript, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	cssText := string(css)
	for _, required := range []string{
		"@media (max-width: 30rem)",
		"@media (prefers-reduced-motion: reduce)",
		"overflow-wrap: anywhere",
		".copy-button",
		"display: none",
		"focus-visible",
	} {
		if !strings.Contains(cssText, required) {
			t.Fatalf("app.css missing %q", required)
		}
	}
	for _, forbidden := range []string{"linear-gradient", "radial-gradient", "letter-spacing: -"} {
		if strings.Contains(cssText, forbidden) {
			t.Fatalf("app.css contains forbidden visual pattern %q", forbidden)
		}
	}

	jsText := string(javascript)
	for _, required := range []string{
		"navigator.clipboard.writeText",
		"command.textContent",
		"status.textContent",
		"previousElementSibling",
		"dataset.copyState",
	} {
		if !strings.Contains(jsText, required) {
			t.Fatalf("app.js missing %q", required)
		}
	}
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(jsText, forbidden) {
			t.Fatalf("app.js contains forbidden DOM sink %q", forbidden)
		}
	}
}
