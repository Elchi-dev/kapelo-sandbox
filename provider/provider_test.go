package provider

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	cases := map[string]struct {
		p    Provider
		want string
	}{
		"preset":       {Provider{BaseURL: "https://openrouter.ai/api/v1", Model: "m"}, ""},
		"local http":   {Provider{BaseURL: "http://localhost:11434/v1", Model: "m"}, ""},
		"remote http":  {Provider{BaseURL: "http://models.example/v1", Model: "m"}, "https"},
		"no model":     {Provider{BaseURL: "https://x.example/v1"}, "no model"},
		"not a URL":    {Provider{BaseURL: "openrouter", Model: "m"}, "not a URL"},
		"unknown kind": {Provider{Kind: "grpc", BaseURL: "https://x.example", Model: "m"}, "kind"},
		"anthropic ok": {Provider{Kind: KindAnthropic, BaseURL: "https://api.anthropic.com/v1", Model: "m"}, ""},
	}
	for name, c := range cases {
		err := c.p.Check()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestKeyAndFile(t *testing.T) {
	env := map[string]string{"OR_KEY": "sk-or-1"}
	get := func(k string) string { return env[k] }
	if k, err := (Provider{APIKeyEnv: "OR_KEY"}).Key(get); k != "sk-or-1" || err != nil {
		t.Errorf("from env: %q %v", k, err)
	}
	if _, err := (Provider{APIKeyEnv: "MISSING"}).Key(get); err == nil {
		t.Error("a missing variable gave a key")
	}
	if k, _ := (Provider{}).Key(get); k == "" {
		t.Error("a provider without a key must still give the harness something")
	}

	path := filepath.Join(t.TempDir(), "kw", "config.json")
	c, err := Load(path)
	if err != nil || len(c.Providers) != 0 {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	if _, _, err := c.Get(""); err == nil || !strings.Contains(err.Error(), "init") {
		t.Errorf("empty config: %v", err)
	}
	c.Providers["or"] = Provider{BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OR_KEY", Model: "m"}
	c.Default = "or"
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	name, p, err := back.Get("")
	if err != nil || name != "or" || p.Kind != KindOpenAI {
		t.Errorf("get default: %s %+v %v", name, p, err)
	}
	if _, _, err := back.Get("nope"); err == nil {
		t.Error("an unknown provider was found")
	}
}
