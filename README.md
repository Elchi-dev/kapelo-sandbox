# kapelo-sandbox

The Kapelo sandbox is one container on your own server or computer where [Kapelo](https://kapelo.app)'s agents work on your projects: they run commands, read and edit files, search the code, start the app and look at it, the way a developer works on a machine of their own. Kapelo itself runs in the cloud; the sandbox is the one part that runs on your hardware, which is why its code is here for you to read.

It also carries a standalone worker that gives one coding task to a model of your choice and checks the result, with no Kapelo involved.

## Start a sandbox

In Kapelo, open the sandbox settings and create a token. Then, on any machine with Docker:

```sh
docker run -d --name kapelo --restart unless-stopped \
  -v kapelo:/data -e KAPELO_TOKEN=kpe_... \
  ghcr.io/elchi-dev/kapelo-sandbox
```

The first start enrolls with the token, which works once, and from then on the container keeps one outbound connection to Kapelo. No port is opened on your machine, and it works behind any router. Projects, the database and the tools you install live in the volume and stay across restarts and updates.

`docker exec kapelo kapelo-sandbox status` shows whether it is online, its projects and what is running. `kapelo-sandbox pause` stops every running command and refuses new ones until `resume`; `docker stop kapelo` ends it altogether.

## What runs in it

Kapelo's agents reach the sandbox only through the connection the sandbox opened, and only with commands Kapelo signed for this sandbox, each valid for a few minutes and accepted once. What they can do:

| Tool | |
|---|---|
| `exec` | run a shell command in a project, up to ten minutes; or start one in the background, such as a dev server, and read its output later |
| `read`, `write`, `edit` | read a file with line numbers, write a whole file, replace an exact piece of one |
| `glob`, `grep` | find files by pattern, search their contents |
| preview | reach an app listening in the sandbox, for Kapelo's preview links |
| git | Kapelo brings the project's repository in and takes commits out as bundles; the sandbox holds no git credentials |

Every project is a folder in `/data/projects` and a git repository. Paths given to the tools stay inside their project; links that lead out are refused. Commands run as uid 1000 with `HOME` in the volume, and with `DATABASE_URL` pointing at a PostgreSQL the sandbox runs for the projects (`KAPELO_POSTGRES=off` turns it off). The image has Node.js 22, git, Python 3, PostgreSQL 15, ripgrep, jq and the usual build tools; anything else can be installed into the home folder.

Model calls do not happen in the sandbox: the agents run in Kapelo, with your organisation's API keys, and only their tool calls come here.

## How much it may do on your machine

That is your choice, made with Docker's own options:

```sh
docker run -d --name kapelo --restart unless-stopped \
  --cpus 4 --memory 8g --pids-limit 2048 \
  --runtime runsc \
  --network kapelo-net \
  -v kapelo:/data -e KAPELO_TOKEN=kpe_... \
  ghcr.io/elchi-dev/kapelo-sandbox
```

`--cpus` and `--memory` cap what the projects' builds and tests can use. `--runtime runsc` runs the container in [gVisor](https://gvisor.dev), with a kernel of its own between it and yours. A network of its own, or firewall rules for the container, decide whether it may reach your local network; it needs outbound HTTPS to Kapelo, and whatever registries your projects install from. The container does not need privileges, the Docker socket, or any folder of yours.

| Variable | |
|---|---|
| `KAPELO_TOKEN` | the enrollment token, for the first start |
| `KAPELO_NAME` | the name shown in Kapelo (default: the container's host name) |
| `KAPELO_SERVER` | Kapelo's endpoint (default `agent.kapelo.app:443`) |
| `KAPELO_POSTGRES` | `off` for no database |
| `KAPELO_DATA` | the data folder (default `/data`) |

## The worker on its own

`kapelo-sandbox worker` gives one task to a model through [OpenCode](https://opencode.ai) on a branch of its own, commits, and checks the result: only the allowed files changed, tests left alone, the project's build, typecheck, lint and tests pass. When an attempt fails, it tries again with the failing check in hand, then with a stronger model. It needs `git` and `opencode` on `PATH`; the image `ghcr.io/elchi-dev/kapelo-worker` has both.

```sh
kapelo-sandbox worker init -provider openrouter -key-env OPENROUTER_API_KEY \
  -model deepseek/deepseek-v4.1-flash -escalate z-ai/glm-5.3

kapelo-sandbox worker run -goal "Add a GET /api/health route that answers {\"ok\": true}" \
  -files "src/app/api/health/**" -criteria "answers 200;answers JSON"
```

Any OpenAI compatible API works (OpenRouter, OpenAI, DeepSeek, Ollama, a gateway of your own), and Anthropic's:

```sh
kapelo-sandbox worker init -provider anthropic -key-env ANTHROPIC_API_KEY -model claude-haiku-5-5
kapelo-sandbox worker init -provider local -base-url http://localhost:11434/v1 -kind openai-compatible -model qwen3-coder
kapelo-sandbox worker init -provider company -base-url https://llm.example.com/v1 -kind openai-compatible \
  -key-env COMPANY_LLM_KEY -model coder-large
```

The configuration lives in `kapelo-worker/config.json` in your user configuration folder (or `KAPELO_WORKER_CONFIG`), readable by you only; keys stay in the environment variable you name. Addresses must be https, except on your own machine.

`run` works in a git repository with nothing uncommitted. The work ends up on `task/<id>`, made from the branch you were on; `-merge` merges it back when the task is done. The exit code is 0 when the task is done and 1 when it is not; every attempt prints a `KAPELO_REPORT {...}` line, and `-report out.json` keeps the last one.

| Flag | |
|---|---|
| `-goal`, `-title`, `-type` | What to do; `-type` is `impl` (default), `impl_hard`, `frontend` or `test` |
| `-files` | Files or patterns the task may change, comma separated; `**` spans folders |
| `-criteria`, `-notes` | Acceptance criteria and notes, separated by `;` |
| `-task task.json` | All of the above as JSON (the `task.Brief` type) |
| `-stack` | A shipped stack by name (`worker stacks`) or a folder with `stack.json` |
| `-provider`, `-model`, `-escalate` | Override the configured provider and models |
| `-attempts` | 1 to 3 steps: the model, the model again with the failure, the stronger model |
| `-base`, `-id`, `-resume`, `-merge` | Branches: start from, name, continue, merge back |

In a container, as your own user so the files stay yours:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" \
  -v "$HOME/.config/kapelo-worker:/config:ro" -e KAPELO_WORKER_CONFIG=/config/config.json -e OPENROUTER_API_KEY \
  ghcr.io/elchi-dev/kapelo-worker run -goal "..." -files "src/**"
```

### Stacks

A stack is a folder with `stack.json`: the rules the model reads with every task, which files are tests, and the checks:

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

`database: postgres` starts a throwaway PostgreSQL for the checks on `127.0.0.1:54329` (`KAPELO_PG_PORT`) unless `DATABASE_URL` is set. Shipped: `next-drizzle` (Next.js, TypeScript, Tailwind, Drizzle on PostgreSQL, Vitest).

## Releases

Binaries for Linux and macOS are on the [releases](https://github.com/Elchi-dev/kapelo-sandbox/releases) page with `SHA256SUMS` signed through Sigstore, and both images are signed the same way:

```sh
cosign verify ghcr.io/elchi-dev/kapelo-sandbox:latest \
  --certificate-identity-regexp '^https://github.com/Elchi-dev/kapelo-sandbox/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The Go packages are usable on their own: `protocol` (the signed commands and the tools' messages), `link`, `sandbox`, `worker`, `gates`, `harness`, `gitrepo`, `provider`, `pgdev`, `stacks` and `task`.

## Licence

MIT, see [LICENSE](LICENSE). Copyright Elchi Studios.
