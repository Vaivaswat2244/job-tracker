package cli

import "testing"

// The stdlib flag package stops at the first positional, so a flag written
// after the URL — which is how every documented invocation reads — was silently
// dropped. Guard the permutation.
func TestParsePermutesFlagsAroundPositionals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantTitle string
		wantSkip  bool
		wantPos   []string
	}{
		{"flags after positional", []string{"https://x/1", "--no-fetch", "--title", "SRE"},
			"SRE", true, []string{"https://x/1"}},
		{"flags before positional", []string{"--title", "SRE", "--no-fetch", "https://x/1"},
			"SRE", true, []string{"https://x/1"}},
		{"flags either side", []string{"--title", "SRE", "https://x/1", "--no-fetch"},
			"SRE", true, []string{"https://x/1"}},
		{"equals form", []string{"https://x/1", "--title=SRE"},
			"SRE", false, []string{"https://x/1"}},
		{"two positionals", []string{"7", "--notes", "n", "in_process"},
			"", false, []string{"7", "in_process"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlags("test")
			title := fs.String("title", "", "")
			notes := fs.String("notes", "", "")
			skip := fs.Bool("no-fetch", false, "")
			_ = notes

			pos, ok := parse(fs, tc.args)
			if !ok {
				t.Fatalf("parse(%v) failed", tc.args)
			}
			if *title != tc.wantTitle {
				t.Errorf("title = %q, want %q", *title, tc.wantTitle)
			}
			if *skip != tc.wantSkip {
				t.Errorf("no-fetch = %v, want %v", *skip, tc.wantSkip)
			}
			if len(pos) != len(tc.wantPos) {
				t.Fatalf("positionals = %v, want %v", pos, tc.wantPos)
			}
			for i := range tc.wantPos {
				if pos[i] != tc.wantPos[i] {
					t.Errorf("positional[%d] = %q, want %q", i, pos[i], tc.wantPos[i])
				}
			}
		})
	}
}

func TestParseRejectsUnknownFlag(t *testing.T) {
	fs := newFlags("test")
	fs.SetOutput(discard{})
	if _, ok := parse(fs, []string{"--nope"}); ok {
		t.Error("parse accepted an unknown flag")
	}
}

// pad and trunc count runes so an em dash in a job title does not shift a column.
func TestPadCountsRunesNotBytes(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{"abc", 5, "abc  "},
		{"Sr — SRE", 10, "Sr — SRE  "},
		{"truncate me", 4, "trun"},
		{"— — — —", 3, "— —"},
	} {
		if got := pad(tc.in, tc.width); got != tc.want {
			t.Errorf("pad(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

func TestOneOf(t *testing.T) {
	allowed := []string{"published", "verified", "inferred"}
	if !oneOf("verified", allowed) {
		t.Error("oneOf missed a member")
	}
	if oneOf("guessed", allowed) {
		t.Error("oneOf accepted a non-member")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
