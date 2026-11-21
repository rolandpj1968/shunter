package tier

import (
	"math/rand"
	"testing"
)

var rules = Rules{
	Inert: []string{"docs/**", "**/*.md", "LICENSE"},
	Read:  []string{"docs/generated/**", "README.md", "**/testdata/**"},
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  Tier
	}{
		{"a document", []string{"docs/design/queue.md"}, Light},
		{"documents in several places", []string{"docs/a.md", "pkg/x/NOTES.md", "LICENSE"}, Light},
		{"a document under docs that is not markdown", []string{"docs/img/flow.svg"}, Light},
		{"code", []string{"pkg/x/x.go"}, Full},
		{"a document and code", []string{"docs/a.md", "pkg/x/x.go"}, Full},
		{"a document something reads", []string{"docs/generated/api.md"}, Full},
		{"the readme a test checks", []string{"README.md"}, Full},
		{"markdown that is a test's data", []string{"pkg/x/testdata/case.md"}, Full},
		{"no files: nothing is known", nil, Full},
		{"a rename out of docs: the old name is a document, the new one is code", []string{"docs/a.md", "pkg/a.go"}, Full},
		{"a path that climbs", []string{"docs/../pkg/x.go"}, Full},
		{"an absolute path", []string{"/docs/a.md"}, Full},
		{"an empty path", []string{"docs/a.md", ""}, Full},
		{"the definition of CI itself", []string{".ci/workflow.yml"}, Full},
	} {
		if got := rules.Classify(tc.files); got != tc.want {
			t.Errorf("%s: %v is %s, want %s", tc.name, tc.files, got, tc.want)
		}
	}
	if (Rules{}).Classify([]string{"docs/a.md"}) != Full {
		t.Error("with no rules a document is light")
	}
	if (Rules{Read: []string{"**"}}).Classify([]string{"docs/a.md"}) != Full {
		t.Error("with no inert pattern a document is light")
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/a/b/c.md", true},
		{"docs/**", "docs", true},
		{"docs/**", "docsx/a.md", false},
		{"**/*.md", "a.md", true},
		{"**/*.md", "x/y/a.md", true},
		{"**/*.md", "x/y/a.mdx", false},
		{"*.md", "x/a.md", false},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c", false},
		{"[", "[", false},
		{"", "", false},
	} {
		if got := Match(tc.pattern, tc.path); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// The classifier can only err towards the full tier. Over random rules and
// random changes:
//   - a light change is one whose every file an inert pattern matches and no
//     read pattern does (checked file by file, apart from Classify);
//   - adding files to a change never lightens it;
//   - adding a read pattern, or removing an inert one, never lightens it.
func TestTheClassifierOnlyErrsTowardsFull(t *testing.T) {
	dirs := []string{"docs", "pkg", "web", "api", "docs/gen", "pkg/testdata", "."}
	names := []string{"a.md", "b.go", "c.sql", "d.md", "e.yaml", "LICENSE", "f.svg"}
	pats := []string{"docs/**", "**/*.md", "*.md", "pkg/**", "**/testdata/**", "docs/gen/**", "LICENSE", "web/*.svg", "**"}
	r := rand.New(rand.NewSource(7))
	file := func() string {
		d := dirs[r.Intn(len(dirs))]
		if d == "." {
			return names[r.Intn(len(names))]
		}
		return d + "/" + names[r.Intn(len(names))]
	}
	some := func(from []string, max int) []string {
		var out []string
		for i := r.Intn(max + 1); i > 0; i-- {
			out = append(out, from[r.Intn(len(from))])
		}
		return out
	}
	lights := 0
	for i := 0; i < 20000; i++ {
		ru := Rules{Inert: some(pats, 3), Read: some(pats, 2)}
		var a, b []string
		for n := r.Intn(4); n >= 0; n-- {
			a = append(a, file())
		}
		for n := r.Intn(3); n > 0; n-- {
			b = append(b, file())
		}
		ta := ru.Classify(a)
		if ta == Light {
			lights++
			for _, f := range a {
				if !matchAny(ru.Inert, f) || matchAny(ru.Read, f) {
					t.Fatalf("%+v: %v is light, and %s is not inert", ru, a, f)
				}
			}
		}
		both := append(append([]string(nil), a...), b...)
		if tb := ru.Classify(both); tb == Light && (ta == Full || (len(b) > 0 && ru.Classify(b) == Full)) {
			t.Fatalf("%+v: %v is light, though a part of it is full", ru, both)
		}
		more := Rules{Inert: ru.Inert, Read: append(append([]string(nil), ru.Read...), pats[r.Intn(len(pats))])}
		if more.Classify(a) == Light && ta == Full {
			t.Fatalf("a read pattern lightened %v: %+v to %+v", a, ru, more)
		}
		if len(ru.Inert) > 0 {
			fewer := Rules{Inert: ru.Inert[1:], Read: ru.Read}
			if fewer.Classify(a) == Light && ta == Full {
				t.Fatalf("removing an inert pattern lightened %v: %+v", a, ru)
			}
		}
	}
	if lights < 100 {
		t.Fatalf("only %d light changes in the walk: it proves little", lights)
	}
}
