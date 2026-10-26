package verwatch

import (
	"context"
	"os"
	"testing"
)

func TestParseDefaultOrder(t *testing.T) {
	text := `{"version":"5.9.1","cli":{"version":"2.170.0"},"name":"WorkBuddy"}`
	r := Parse(text, Source{})
	if r.Client != "5.9.1" {
		t.Errorf("client=%q want 5.9.1", r.Client)
	}
	if r.CLI != "2.170.0" {
		t.Errorf("cli=%q want 2.170.0", r.CLI)
	}
}

func TestParseCustomRegex(t *testing.T) {
	text := "app 5.10.0 | cli 2.180.3"
	r := Parse(text, Source{
		ClientRegex: `app\s+(\d+\.\d+\.\d+)`,
		CLRegex:     `cli\s+(\d+\.\d+\.\d+)`,
	})
	if r.Client != "5.10.0" || r.CLI != "2.180.3" {
		t.Fatalf("got %q / %q", r.Client, r.CLI)
	}
}

func TestParseSingleVersion(t *testing.T) {
	r := Parse(`<html>latest 5.9.2</html>`, Source{})
	if r.Client != "5.9.2" {
		t.Errorf("client=%q", r.Client)
	}
	if r.CLI != "" {
		t.Errorf("只有一条版本号时不应猜 CLI，得到 %q", r.CLI)
	}
}

func TestParseNoVersion(t *testing.T) {
	r := Parse("nothing here", Source{})
	if r.Client != "" || r.CLI != "" {
		t.Fatalf("应当解析不到版本: %+v", r)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"5.9.1", "5.9.0", true},
		{"5.9.0", "5.9.0", false},
		{"5.9.0", "5.9.1", false},
		{"6.0.0", "5.99.99", true},
		{"5.10.0", "5.9.9", true},
		{"5.9.1-12", "5.9.0", true},
		{"", "5.9.0", false},
		{"5.9.0", "", true},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFetchLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/product.json"
	if err := os.WriteFile(path, []byte(`{"version":"5.9.9","cli":"2.170.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := Fetch(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	r := Parse(text, Source{})
	if r.Client != "5.9.9" || r.CLI != "2.170.0" {
		t.Fatalf("parse %+v", r)
	}
}
