package skills

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Ref
	}{
		{
			name: "plain URL",
			in:   "https://github.com/acme/skills",
			want: Ref{URL: "https://github.com/acme/skills"},
		},
		{
			name: "URL with ref",
			in:   "https://github.com/acme/skills@v1.2",
			want: Ref{URL: "https://github.com/acme/skills", Ref: "v1.2"},
		},
		{
			name: "URL with sub-directory",
			in:   "https://github.com/acme/skills#skills/tdd",
			want: Ref{URL: "https://github.com/acme/skills", Subdir: "skills/tdd"},
		},
		{
			name: "URL with ref and sub-directory",
			in:   "https://github.com/acme/skills@v1.2#skills/tdd",
			want: Ref{URL: "https://github.com/acme/skills", Ref: "v1.2", Subdir: "skills/tdd"},
		},
		{
			name: "scp-style without ref",
			in:   "git@host:org/repo",
			want: Ref{URL: "git@host:org/repo"},
		},
		{
			name: "scp-style with ref",
			in:   "git@host:org/repo@v1",
			want: Ref{URL: "git@host:org/repo", Ref: "v1"},
		},
		{
			name: "scp-style with ref and sub-directory",
			in:   "git@host:org/repo@v1#skills/tdd",
			want: Ref{URL: "git@host:org/repo", Ref: "v1", Subdir: "skills/tdd"},
		},
		{
			name: "https URL with a port",
			in:   "https://git.example.com:8443/acme/skills@v1.2#skills/tdd",
			want: Ref{URL: "https://git.example.com:8443/acme/skills", Ref: "v1.2", Subdir: "skills/tdd"},
		},
		{
			name: "https URL with user@ in it",
			in:   "https://user@git.example.com/acme/skills@v1.2",
			want: Ref{URL: "https://user@git.example.com/acme/skills", Ref: "v1.2"},
		},
		{
			name: "https URL with user@ and no ref",
			in:   "https://user@git.example.com/acme/skills",
			want: Ref{URL: "https://user@git.example.com/acme/skills"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty reference", ""},
		{"whitespace-only reference", "   "},
		{"empty URL with ref", "@v1"},
		{"empty URL with sub-directory", "#skills/tdd"},
		{"absolute sub-directory", "https://github.com/acme/skills#/etc/passwd"},
		{"sub-directory climbing above root directly", "https://github.com/acme/skills#../x"},
		{"sub-directory climbing above root after cleaning", "https://github.com/acme/skills#a/../../x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(c.in); err == nil {
				t.Fatalf("Parse(%q): expected error, got none", c.in)
			}
		})
	}
}

func TestRefStringDistinctForDifferentOwner(t *testing.T) {
	a, err := Parse("https://github.com/acme/skills")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("https://github.com/other/skills")
	if err != nil {
		t.Fatal(err)
	}
	if a.String() == b.String() {
		t.Fatalf("expected distinct full-reference strings, both got %q", a.String())
	}
}

func TestRefStringDistinctForDifferentRef(t *testing.T) {
	a, err := Parse("https://github.com/acme/skills@v1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("https://github.com/acme/skills@v2")
	if err != nil {
		t.Fatal(err)
	}
	if a.String() == b.String() {
		t.Fatalf("expected distinct full-reference strings, both got %q", a.String())
	}
}

func TestRefStringDistinctForDifferentSubdir(t *testing.T) {
	a, err := Parse("https://github.com/acme/skills#skills/a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("https://github.com/acme/skills#skills/b")
	if err != nil {
		t.Fatal(err)
	}
	if a.String() == b.String() {
		t.Fatalf("expected distinct full-reference strings, both got %q", a.String())
	}
}
