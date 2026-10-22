# Changelog

## Unreleased

- The repository is now kapelo-sandbox: the Kapelo sandbox, one long running container on your own hardware where Kapelo's agents run commands (`exec`, in the foreground or the background), read, write and edit files, search with `glob` and `grep`, and reach the apps they start. One outbound link with yamux over TLS, enrollment with a one time token, commands signed by Kapelo for this sandbox only, certificate renewal, projects as git repositories in `/data/projects` with bundles in and out, a PostgreSQL for the projects, `status`, `pause` and `resume`.
- The worker is now `kapelo-sandbox worker init|run|stacks`; its image is `ghcr.io/elchi-dev/kapelo-worker`. The one attempt mode with `KAPELO_TASK` is gone: Kapelo runs its agents itself.
- Module path `github.com/Elchi-dev/kapelo-sandbox`. New packages: `protocol`, `link`, `sandbox`, `pgdev`.
- The memory reported at enrollment is the container's limit on cgroup v1 hosts too, not the machine's.

## v0.1.0

- `kapelo-worker run`: one task in a git repository with up to three attempts (the model on a fresh branch, the same model with the failing check, the stronger model on a fresh branch), the work on `task/<id>`, `-merge` to merge it back.
- `kapelo-worker init`: providers with an OpenAI compatible API or Anthropic's, presets for OpenRouter, OpenAI, Anthropic, DeepSeek and Ollama, keys kept in the environment.
- Checks: `protected`, `scope`, `test-budget` and the stack's own; a throwaway PostgreSQL for stacks that need one, also as a user without a passwd entry.
- The `next-drizzle` stack (Next.js, TypeScript, Tailwind, Drizzle on PostgreSQL, Vitest).
- One attempt from `KAPELO_TASK` with a `KAPELO_REPORT` line, for schedulers.
- The image `ghcr.io/elchi-dev/kapelo-sandbox` with OpenCode 1.19.3, git, Node.js 22 and PostgreSQL 15, signed with Sigstore.
