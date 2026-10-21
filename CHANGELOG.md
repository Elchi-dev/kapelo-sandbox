# Changelog

## v0.1.0

- `kapelo-worker run`: one task in a git repository with up to three attempts (the model on a fresh branch, the same model with the failing check, the stronger model on a fresh branch), the work on `task/<id>`, `-merge` to merge it back.
- `kapelo-worker init`: providers with an OpenAI compatible API or Anthropic's, presets for OpenRouter, OpenAI, Anthropic, DeepSeek and Ollama, keys kept in the environment.
- Checks: `protected`, `scope`, `test-budget` and the stack's own; a throwaway PostgreSQL for stacks that need one, also as a user without a passwd entry.
- The `next-drizzle` stack (Next.js, TypeScript, Tailwind, Drizzle on PostgreSQL, Vitest).
- One attempt from `KAPELO_TASK` with a `KAPELO_REPORT` line, for schedulers.
- The image `ghcr.io/elchi-dev/kapelo-worker` with OpenCode 1.19.3, git, Node.js 22 and PostgreSQL 15, signed with Sigstore.
