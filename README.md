# kapelo-worker

kapelo-worker gives one coding task to a model and checks the result before you look at it. You name the goal and the files the task may change; the worker runs [OpenCode](https://opencode.ai) with a model of your choice on a branch of its own, commits, and runs the checks: only the allowed files changed, tests left alone, and the project's own build, typecheck, lint and tests pass. When an attempt fails, it tries again with the failing check in hand, then with a stronger model.

It is the worker [Kapelo](https://kapelo.app) runs in its sandboxes, and it works just as well on its own: in a terminal, in CI, or behind a scheduler of your own.

## How an attempt goes

1. **Prepare.** The stack's prepare step runs, such as `npm ci`. What it creates stays out of the task's commit.
2. **Work.** OpenCode gets the task card: goal, acceptance criteria, allowed files, the stack's rules, notes, and on a second try the failing check. It may edit files and run commands; it may not browse the web.
3. **Commit.** Everything the model changed is committed on `task/<id>`.
4. **Check.** `protected` (an implementation task does not touch tests), `scope` (only allowed files changed), `test-budget` (a test task stays within its number of tests), then the stack's checks in order. The first failure stops the rest.

`run` climbs a ladder of up to three attempts: your model on a fresh branch, the same model on its own work with the failure, the stronger model on a fresh branch.

## Install

Binaries for Linux and macOS are on the [releases](https://github.com/Elchi-dev/kapelo-worker/releases) page, with `SHA256SUMS` signed through Sigstore:

```sh
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/Elchi-dev/kapelo-worker/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c SHA256SUMS --ignore-missing
```

Or with Go 1.26 or later:

```sh
go install github.com/Elchi-dev/kapelo-worker/cmd/kapelo-worker@latest
```

The worker needs `git` and `opencode` (`npm i -g opencode-ai`) on `PATH`, and PostgreSQL's `pg_ctl` when a stack's checks need a database and `DATABASE_URL` is not set. The image below has all of them.

## Choose a provider

Any OpenAI compatible API works (OpenRouter, OpenAI, DeepSeek, Ollama, a gateway of your own), and Anthropic's. `init` asks for what it needs:

```sh
kapelo-worker init
```

or takes it all as flags:

```sh
kapelo-worker init -provider openrouter -key-env OPENROUTER_API_KEY \
  -model deepseek/deepseek-v4.1-flash -escalate z-ai/glm-5.3

kapelo-worker init -provider anthropic -key-env ANTHROPIC_API_KEY -model claude-haiku-5-5

kapelo-worker init -provider local -base-url http://localhost:11434/v1 -kind openai-compatible -model qwen3-coder

kapelo-worker init -provider company -base-url https://llm.example.com/v1 -kind openai-compatible \
  -key-env COMPANY_LLM_KEY -model coder-large
```

The configuration lives in `kapelo-worker/config.json` in your user configuration folder (or wherever `KAPELO_WORKER_CONFIG` points), readable by you only. Keys stay in the environment variable you name; `-key-env` is the name, not the key. Addresses must be https, except on your own machine.

## Run a task

In a git repository with nothing uncommitted:

```sh
kapelo-worker run -goal "Add a GET /api/health route that answers {\"ok\": true}" \
  -files "src/app/api/health/**" -criteria "answers 200;answers JSON"
```

The work ends up on `task/<id>`, made from the branch you were on. `-merge` merges it back when the task is done. The exit code is 0 when the task is done and 1 when it is not; every attempt prints a report line, and `-report out.json` keeps the last one.

| Flag | |
|---|---|
| `-goal`, `-title`, `-type` | What to do; `-type` is `impl` (default), `impl_hard`, `frontend` or `test` |
| `-files` | Files or patterns the task may change, comma separated; `**` spans folders |
| `-criteria`, `-notes` | Acceptance criteria and notes, separated by `;` |
| `-task task.json` | All of the above as JSON, in the format below |
| `-stack` | A shipped stack by name (`kapelo-worker stacks`) or a folder with `stack.json` |
| `-provider`, `-model`, `-escalate` | Override the configured provider and models |
| `-attempts` | 1 to 3 steps of the ladder |
| `-base`, `-id`, `-resume`, `-merge` | Branches: start from, name, continue, merge back |

A test task writes tests against behaviour that may not exist yet; every other type changes the implementation and treats the stack's test files as read only.

## In a container

The image carries the worker, OpenCode, git, Node.js and PostgreSQL, and runs as uid 1000:

```sh
docker run --rm -v "$PWD:/work" -v "$HOME/.config/kapelo-worker:/config:ro" \
  -e KAPELO_WORKER_CONFIG=/config/config.json -e OPENROUTER_API_KEY \
  ghcr.io/elchi-dev/kapelo-worker run -goal "..." -files "src/**"
```

The model runs shell commands in the repository. Running it in a container keeps those commands away from the rest of your machine.

## Stacks

A stack is a folder with `stack.json`:

```json
{
  "name": "my-stack",
  "rules": "rules.md",
  "test_globs": ["**/*.test.ts"],
  "always_allowed": ["package.json", "package-lock.json"],
  "prepare": [{ "name": "install", "cmd": "test -d node_modules || npm ci" }],
  "database": "postgres",
  "gates": {
    "impl": [{ "name": "build", "cmd": "npm run build", "timeout_minutes": 10 }, { "name": "test", "cmd": "npm test" }],
    "test": [{ "name": "lint", "cmd": "npx eslint ." }]
  }
}
```

`rules` is a Markdown file the model reads with every task. `database: postgres` starts a throwaway PostgreSQL for the checks on `127.0.0.1:54329` (`KAPELO_PG_PORT`) and sets `DATABASE_URL`, unless it is set already. `setup`, `final` and `line_exts` are used by tools that scaffold and review whole projects. Point `-stack` at the folder, or put stacks into the folder `KAPELO_STACKS` names.

Shipped: `next-drizzle` (Next.js, TypeScript, Tailwind, Drizzle on PostgreSQL, Vitest).

## From a scheduler of your own

Without a command, the worker runs one attempt of the task in `KAPELO_TASK` against the API at `KAPELO_LLM_BASE`, in the clone at `/work` (`KAPELO_WORKDIR`). It never picks a model or a branch itself; whatever starts it decides. It ends with one line:

```
KAPELO_REPORT {"status":"done","model":"m","attempt":1,"summary":"...","gates":{"scope":true,"build":true}}
```

`status` is `done`, `failed` or `blocked`. `summary` and `blockers` are short, for whoever plans the work; `failure` is the failing check's output, to hand to the next attempt as `previous_failure`. The exit code is 0 once the report is written; anything else means the worker itself broke. The brief:

```json
{
  "task": 12, "title": "Health route", "type": "impl",
  "goal": "Add GET /api/health", "acceptance_criteria": ["answers 200"],
  "allowed_files": ["src/app/api/health/**"], "notes": [], "decisions": [],
  "attempt": 1, "model": "deepseek/deepseek-v4.1-flash", "models": ["deepseek/deepseek-v4.1-flash"],
  "stack": "next-drizzle", "test_budget": 0, "previous_failure": ""
}
```

The packages are usable from Go as well: `task` (brief and report), `worker` (one attempt), `gates`, `harness`, `gitrepo`, `provider` and `stacks`.

## Licence

MIT, see [LICENSE](LICENSE). Copyright Elchi Studios.
