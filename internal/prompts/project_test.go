package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// writeProjectPrompt creates repoDir/bees/prompts/<name> with body.
func writeProjectPrompt(t *testing.T, repoDir, name, body string) {
	t.Helper()
	dir := filepath.Join(repoDir, filepath.FromSlash(ProjectDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A project that keeps role instructions in bees/prompts/ has them appended
// to the role's system prompt, common.md before <role>.md and both after the
// text bees.toml configures. The order is what makes bees.toml the place for
// a machine-specific override.
func TestProjectPromptFilesAreAppendedInOrder(t *testing.T) {
	repo := t.TempDir()
	writeProjectPrompt(t, repo, "common.md", "Every role: speak plainly.")
	writeProjectPrompt(t, repo, "developer.md", "Developers: run make lint.")
	writeProjectPrompt(t, repo, "reviewer.md", "Reviewers: check the migrations.")

	project, err := LoadProject(repo, config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	sys, err := System(config.RoleDeveloper, sample(), "from bees.toml", project...)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"## Additional instructions from bees.toml",
		"from bees.toml",
		"## Additional instructions from bees/prompts/common.md",
		"Every role: speak plainly.",
		"## Additional instructions from bees/prompts/developer.md",
		"Developers: run make lint.",
	}
	at := -1
	for _, w := range want {
		i := strings.Index(sys, w)
		if i < 0 {
			t.Fatalf("developer prompt is missing %q:\n%s", w, sys)
		}
		if i < at {
			t.Errorf("%q renders before the text it must follow:\n%s", w, sys)
		}
		at = i
	}
	// Another role's file belongs to that role alone.
	if strings.Contains(sys, "check the migrations") {
		t.Errorf("developer prompt carries the reviewer's project prompt:\n%s", sys)
	}

	rev, err := LoadProject(repo, config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != 2 || rev[0].Path != "bees/prompts/common.md" || rev[1].Path != "bees/prompts/reviewer.md" {
		t.Errorf("reviewer project prompts: %+v", rev)
	}
}

// Every repository that has never heard of this feature has no bees/prompts/
// directory, so that case must render byte for byte the prompt bees rendered
// before it existed - and read the directory silently, with no error.
func TestNoProjectPromptsRendersTheSamePromptAsBefore(t *testing.T) {
	repo := t.TempDir()
	for _, role := range config.Roles {
		project, err := LoadProject(repo, role)
		if err != nil {
			t.Fatalf("%s: reading a repository with no %s must not fail: %v", role, ProjectDir, err)
		}
		if len(project) != 0 {
			t.Fatalf("%s: %+v", role, project)
		}
		with, err := System(role, sample(), "custom instructions here", project...)
		if err != nil {
			t.Fatal(err)
		}
		without, err := System(role, sample(), "custom instructions here")
		if err != nil {
			t.Fatal(err)
		}
		if with != without {
			t.Errorf("%s system prompt changed for a repository with no %s/", role, ProjectDir)
		}
		if strings.Contains(with, "Additional instructions from bees/") {
			t.Errorf("%s system prompt names a project prompt file that does not exist:\n%s", role, with)
		}
	}
}

// A file bees cannot use is reported, and the files it can are still returned:
// the session skips the broken one rather than losing its whole prompt.
func TestUnreadableProjectPromptIsReportedButDoesNotHideTheRest(t *testing.T) {
	repo := t.TempDir()
	writeProjectPrompt(t, repo, "common.md", "Every role: speak plainly.")
	writeProjectPrompt(t, repo, "developer.md", strings.Repeat("x", MaxProjectPromptBytes+1))

	project, err := LoadProject(repo, config.RoleDeveloper)
	if err == nil {
		t.Fatal("an oversized project prompt file must be reported")
	}
	if !strings.Contains(err.Error(), "bees/prompts/developer.md") || !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not name the file and why: %v", err)
	}
	if len(project) != 1 || project[0].Path != "bees/prompts/common.md" {
		t.Errorf("the readable file must still be returned, got %+v", project)
	}
}

// An empty file is not a section: rendering a heading with nothing under it
// would tell a session something is configured when nothing is.
func TestEmptyProjectPromptIsDropped(t *testing.T) {
	repo := t.TempDir()
	writeProjectPrompt(t, repo, "common.md", "\n   \n")
	project, err := LoadProject(repo, config.RoleQA)
	if err != nil {
		t.Fatal(err)
	}
	if len(project) != 0 {
		t.Fatalf("empty file rendered as a prompt: %+v", project)
	}
}

// ProjectPromptFiles is what `bees doctor` reports from: it has to name the
// files no role will ever read, which is how a misspelled role name is found.
func TestProjectPromptFilesSplitsKnownFromUnknown(t *testing.T) {
	repo := t.TempDir()
	known, unknown, err := ProjectPromptFiles(repo)
	if err != nil || known != nil || unknown != nil {
		t.Fatalf("a missing directory must read as nothing: %v %v %v", known, unknown, err)
	}

	writeProjectPrompt(t, repo, "common.md", "x")
	writeProjectPrompt(t, repo, "developer.md", "x")
	writeProjectPrompt(t, repo, "develloper.md", "x")
	writeProjectPrompt(t, repo, "notes.txt", "x")
	known, unknown, err = ProjectPromptFiles(repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(known, " ") != "bees/prompts/common.md bees/prompts/developer.md" {
		t.Errorf("known: %v", known)
	}
	if strings.Join(unknown, " ") != "bees/prompts/develloper.md" {
		t.Errorf("unknown: %v", unknown)
	}
}
