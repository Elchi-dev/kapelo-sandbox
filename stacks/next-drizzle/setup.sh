#!/usr/bin/env bash
# Scaffolds the next-drizzle starter stack into the current, empty folder:
# Next.js with TypeScript and Tailwind, Drizzle on PostgreSQL, Vitest.
# Deterministic, no model involved.
set -euo pipefail

PG_PORT="${KAPELO_PG_PORT:-54329}"
NAME="${KAPELO_APP:-$(basename "$PWD")}"

npx --yes create-next-app@latest . --ts --tailwind --eslint --app --src-dir \
  --import-alias "@/*" --use-npm --no-turbopack --yes --skip-install </dev/null || \
npx --yes create-next-app@latest . --ts --tailwind --eslint --app --src-dir \
  --import-alias "@/*" --use-npm --yes --skip-install </dev/null

npm install --no-audit --no-fund drizzle-orm postgres zod
npm install --no-audit --no-fund -D drizzle-kit vitest @types/node@^22

npm pkg set \
  scripts.typecheck="next typegen && tsc --noEmit" \
  scripts.test="vitest run" \
  scripts.db:migrate="drizzle-kit push --force"

cat > compose.yaml <<EOF
services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: app
      POSTGRES_DB: app
    ports:
      - "127.0.0.1:${PG_PORT}:5432"
    tmpfs:
      - /var/lib/postgresql/data
EOF

cat > .env <<EOF
DATABASE_URL=postgres://app:app@127.0.0.1:${PG_PORT}/app
EOF
# The database is local and throwaway; the .env holds no secret.
sed -i '/^\.env\*$/d;/^\.env$/d' .gitignore
printf '\n# local env files other than .env\n.env.local\n.env*.local\n' >> .gitignore

mkdir -p src/db
cat > src/db/schema.ts <<'EOF'
// Database schema. Every table lives here.
export {};
EOF

cat > src/db/index.ts <<'EOF'
import { drizzle } from "drizzle-orm/postgres-js";
import postgres from "postgres";
import * as schema from "./schema";

const url = process.env.DATABASE_URL;
if (!url) {
  throw new Error("DATABASE_URL is not set");
}

const client = postgres(url, { max: 5 });
export const db = drizzle(client, { schema });
EOF

cat > drizzle.config.ts <<'EOF'
import { defineConfig } from "drizzle-kit";

process.loadEnvFile?.(".env");

export default defineConfig({
  schema: "./src/db/schema.ts",
  dialect: "postgresql",
  dbCredentials: { url: process.env.DATABASE_URL ?? "" },
});
EOF

cat > vitest.config.mts <<'EOF'
import { defineConfig } from "vitest/config";
import { loadEnv } from "vite";
import path from "node:path";

export default defineConfig({
  test: {
    env: loadEnv("test", process.cwd(), ""),
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    fileParallelism: false,
    passWithNoTests: true,
  },
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "src") },
  },
});
EOF

# Gates must hold on the scaffold, or every task fails for nothing.
if docker info >/dev/null 2>&1; then
  docker compose -p "kapelo-${NAME}" up -d --wait
else
  echo "warning: docker is not running; start PostgreSQL on 127.0.0.1:${PG_PORT} before the run" >&2
fi
npm run typecheck
npm run lint -- --max-warnings 0
npm run build
npm test
echo "stack next-drizzle ready"
