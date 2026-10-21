package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Elchi-dev/kapelo-worker/provider"
)

func TestConfig(t *testing.T) {
	var cfg struct {
		Provider map[string]struct {
			NPM     string            `json:"npm"`
			Options map[string]string `json:"options"`
			Models  map[string]any    `json:"models"`
		} `json:"provider"`
		Model string `json:"model"`
	}
	raw := Config(Endpoint{Kind: provider.KindAnthropic, BaseURL: "https://api.anthropic.com/v1", APIKey: "k"}, "claude-x", []string{"claude-y"}, 1000)
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Provider["kapelo"]
	if p.NPM != "@ai-sdk/anthropic" || p.Options["apiKey"] != "k" || len(p.Models) != 2 || cfg.Model != "kapelo/claude-x" {
		t.Errorf("config %s", raw)
	}
}

func TestSummary(t *testing.T) {
	out := `{"type":"step_start"}
{"type":"text","part":{"type":"text","text":"First thought."}}
not json
{"type":"text","part":{"type":"text","text":"Wrote page.txt. Nothing blocks me."}}`
	if s := Summary(out); s != "Wrote page.txt. Nothing blocks me." {
		t.Errorf("summary %q", s)
	}
}

// TestLiveOpenCode runs the real OpenCode binary against a fake OpenAI
// compatible API that asks it to write a file. It proves the harness
// config is accepted, calls carry the key, tools work, and the summary is
// found in the JSON events.
// Run with KAPELO_LIVE_OPENCODE=1; it needs opencode on PATH.
func TestLiveOpenCode(t *testing.T) {
	if os.Getenv("KAPELO_LIVE_OPENCODE") == "" {
		t.Skip("set KAPELO_LIVE_OPENCODE=1 to run against the real harness")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not installed")
	}
	dir := t.TempDir()
	var mu sync.Mutex
	var toolNames, auth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		if len(toolNames) == 0 {
			for _, tl := range req.Tools {
				toolNames = append(toolNames, tl.Function.Name)
			}
		}
		mu.Unlock()
		hasTool := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasTool = true
			}
		}
		usage := `"usage":{"prompt_tokens":800,"completion_tokens":30,"cost":0.001,"prompt_tokens_details":{"cached_tokens":200}}`
		var chunks []string
		switch {
		case len(req.Tools) == 0:
			// A side call, such as a title: answer plainly.
			chunks = []string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Write hello"},"finish_reason":"stop"}],` + usage + `}`}
		case !hasTool:
			args, _ := json.Marshal(map[string]string{"filePath": filepath.Join(dir, "hello.txt"), "content": "hello from the worker\n"})
			chunks = []string{
				fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":%q}}]}}]}`, string(args)),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],` + usage + `}`,
			}
		default:
			chunks = []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Wrote hello.txt. Nothing blocks me."}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` + usage + `}`,
			}
		}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"Write hello"},"finish_reason":"stop"}],%s}`, req.Model, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			c = strings.Replace(c, "{", fmt.Sprintf(`{"id":"x","object":"chat.completion.chunk","created":1,"model":%q,`, req.Model), 1)
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	h := Harness{Command: "opencode", Timeout: 3 * time.Minute, ContextSize: 128000}
	res := h.Run(context.Background(), dir, Endpoint{Kind: provider.KindOpenAI, BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
		"m/std", []string{"m/std", "m/big"}, "Create hello.txt with a greeting.")
	t.Logf("tools offered: %v", toolNames)
	if res.Err != nil {
		t.Fatalf("harness: %v\n%s", res.Err, res.Log)
	}
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || !strings.Contains(string(b), "hello from the worker") {
		t.Fatalf("hello.txt = %q, %v\n%s", b, err, res.Log)
	}
	if !strings.Contains(res.Summary, "Wrote hello.txt") {
		t.Errorf("summary = %q\nlog:\n%s", res.Summary, res.Log)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range auth {
		if a != "Bearer sk-test" {
			t.Errorf("a call carried %q", a)
		}
	}
}
