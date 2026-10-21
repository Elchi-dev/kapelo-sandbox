package gates

import "testing"

// Scope decides whether a task's work is kept, so a wrong match either
// throws away good work or lets a worker change files it must not. Next.js
// route folders contain brackets and parentheses; a pattern built without
// quoting them would match the wrong files.
func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"src/app/api/**", "src/app/api/projects/route.ts", true},
		{"src/app/api/**", "src/app/page.tsx", false},
		{"**/*.test.ts", "route.test.ts", true},
		{"**/*.test.ts", "src/a/b/route.test.ts", true},
		{"**/*.test.ts", "src/a/route.ts", false},
		{"src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/x/a.ts", false},
		{"src/db/schema.ts", "src/db/schema.ts", true},
		{"src/db/schema.ts", "src/db/schema.tsx", false},
		{"src/(auth)/**", "src/(auth)/login/page.tsx", true},
		{"src/[id]/page.tsx", "src/[id]/page.tsx", true},
		{"src/[id]/page.tsx", "src/i/page.tsx", false},
	}
	for _, c := range cases {
		g, err := CompileGlob(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Match(c.path); got != c.want {
			t.Errorf("%q matches %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// The test budget gate counts added test cases; it must not count calls
// that only look similar.
func TestTestCallCount(t *testing.T) {
	cases := map[string]int{
		`it("works", () => {})`:                   1,
		`test("a", async () => {}); it("b", f)`:   2,
		`it.each([1, 2])("case %d", (n) => {})`:   1,
		`const split = s.split(",")`:              0,
		`// submit("x")`:                          0,
		`describe("POST /api/clients", () => {`:   0,
		`  it  ("spaced", () => {})`:              1,
		`  test.skip("not counted twice", f)`:     0,
		`expect(await waitFor(() => it))`:         0,
		`await test("nested in await", () => {})`: 1,
	}
	for line, want := range cases {
		if got := len(testCall.FindAllString(line, -1)); got != want {
			t.Errorf("%q: %d tests, want %d", line, got, want)
		}
	}
}
