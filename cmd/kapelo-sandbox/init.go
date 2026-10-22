package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/Elchi-dev/kapelo-sandbox/provider"
)

// initCmd sets up a provider. Every flag that is missing is asked for;
// with all of them given it asks nothing, for scripts and images.
func initCmd(e env, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	name := fs.String("provider", "", "name: "+strings.Join(provider.PresetNames(), ", ")+", or a name of your own")
	baseURL := fs.String("base-url", "", "API address (OpenAI compatible, or Anthropic's)")
	kind := fs.String("kind", "", "openai-compatible or anthropic")
	keyEnv := fs.String("key-env", "", "environment variable that holds the API key")
	model := fs.String("model", "", "model for the first two attempts")
	escalate := fs.String("escalate", "", "stronger model for the third attempt (optional)")
	noDefault := fs.Bool("no-default", false, "do not make this provider the default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := provider.Path(e.getenv)
	cfg, err := provider.Load(path)
	if err != nil {
		return err
	}
	ask := func(question, def string) (string, error) {
		if def != "" {
			fmt.Fprintf(e.errw, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(e.errw, "%s: ", question)
		}
		line, err := e.in.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil && line == "" && def == "" {
			return "", errors.New("no answer on standard input; give the flags instead")
		}
		if line == "" {
			return def, nil
		}
		return line, nil
	}

	if *name == "" {
		fmt.Fprintf(e.errw, "Providers: %s, or any name for your own API.\n", strings.Join(provider.PresetNames(), ", "))
		if *name, err = ask("Provider", "openrouter"); err != nil {
			return err
		}
	}
	p, preset := provider.Presets[*name]
	if old, ok := cfg.Providers[*name]; ok {
		p = old
	}
	if *kind != "" {
		p.Kind = *kind
	}
	if *baseURL != "" {
		p.BaseURL = *baseURL
	} else if p.BaseURL == "" || !preset {
		if p.BaseURL, err = ask("API address", p.BaseURL); err != nil {
			return err
		}
	}
	if p.Kind == "" {
		if p.Kind, err = ask("Kind (openai-compatible or anthropic)", provider.KindOpenAI); err != nil {
			return err
		}
	}
	switch {
	case *keyEnv != "":
		p.APIKeyEnv, p.APIKey = *keyEnv, ""
	case p.APIKeyEnv == "" && p.APIKey == "" && *name != "ollama":
		if p.APIKeyEnv, err = ask("Environment variable with the API key (empty for none)", strings.ToUpper(slugUnderscore(*name))+"_API_KEY"); err != nil {
			return err
		}
	}
	if *model != "" {
		p.Model = *model
	} else if p.Model, err = ask("Model for the first attempts", p.Model); err != nil {
		return err
	}
	if *escalate != "" {
		p.Escalate = *escalate
	} else if len(args) == 0 {
		if p.Escalate, err = ask("Stronger model for the third attempt (empty for the same)", p.Escalate); err != nil {
			return err
		}
	}
	if err := p.Check(); err != nil {
		return err
	}
	cfg.Providers[*name] = p
	if !*noDefault || cfg.Default == "" {
		cfg.Default = *name
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "Saved provider %s in %s.\n", *name, path)
	if p.APIKeyEnv != "" && e.getenv(p.APIKeyEnv) == "" {
		fmt.Fprintf(e.out, "Set %s before kapelo-sandbox worker run.\n", p.APIKeyEnv)
	}
	return nil
}

func slugUnderscore(s string) string { return strings.ReplaceAll(slug(s), "-", "_") }

func minutesOf(n int) time.Duration {
	if n <= 0 {
		n = 20
	}
	return time.Duration(n) * time.Minute
}
