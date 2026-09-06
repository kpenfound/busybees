package config

import (
	"fmt"
	"slices"
	"strings"
)

// Template is a named way to run bees on a project: the settings that
// define how the factory runs, and nothing project-specific. Repo, branch,
// filter, models, budgets and the like are what RenderTOML writes on its own;
// a template only decides which of the eight keys in TemplateKeys are active
// in the file it renders, and with what value.
type Template struct {
	Name    string // "issue-driven"
	Summary string // one line, for `bees templates list`
	When    string // a paragraph: who it is for and what a person does
	// Settings maps a bees.toml key path to the TOML value the template
	// writes for it, e.g. "roles.qa.enabled" -> "false". Every key is one of
	// TemplateKeys, and every template sets all of them, so the decisions a
	// template makes are visible in the file rather than inherited silently.
	Settings map[string]string
}

// TemplateKeys are the bees.toml keys a template may set, in the order the
// file writes them. They are the only lines RenderTOML can activate.
func TemplateKeys() []string {
	return slices.Clone(templateKeys)
}

var templateKeys = []string{
	"scheduler.review_assigned_prs",
	"scheduler.feature_proposals",
	"roles.product_manager.enabled",
	"roles.project_manager.enabled",
	"roles.developer.enabled",
	"roles.reviewer.enabled",
	"roles.reviewer.auto_merge",
	"roles.qa.enabled",
}

// settings builds a template's Settings from its eight decisions: the five
// roles in Roles order, auto-merge, feature proposals, assigned-PR reviews.
func settings(productManager, projectManager, developer, reviewer, qa, autoMerge, featureProposals, reviewAssignedPRs bool) map[string]string {
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return map[string]string{
		"roles.product_manager.enabled": b(productManager),
		"roles.project_manager.enabled": b(projectManager),
		"roles.developer.enabled":       b(developer),
		"roles.reviewer.enabled":        b(reviewer),
		"roles.qa.enabled":              b(qa),
		"roles.reviewer.auto_merge":     b(autoMerge),
		"scheduler.feature_proposals":   b(featureProposals),
		"scheduler.review_assigned_prs": b(reviewAssignedPRs),
	}
}

// templates is the registry, in the order Templates returns them: the
// smallest footprint on a project first, then the full staff, then the two
// that build nothing.
var templates = []Template{
	{
		Name:    "contributor",
		Summary: "one contributor among many: builds the issues in its filter, a person merges",
		When: "bees is one contributor among many on the project. It takes the issues " +
			"in its filter, sizes them, builds each one on a branch and opens a pull " +
			"request that a person reviews and merges. The product manager and QA are " +
			"off, so nothing proposes features or files bugs; the project manager is " +
			"on, so a person's issue is triaged into a ready work item without any " +
			"further help.",
		Settings: settings(false, true, true, true, false, false, true, false),
	},
	{
		Name:    "issue-driven",
		Summary: "a person writes feature issues, the full staff builds them, a person merges",
		When: "A person writes feature issues and the full staff takes them from there: " +
			"the product manager breaks each one into work items, the project manager " +
			"triages them, developers and reviewers build them, and QA tests the " +
			"default branch after every merge. A person approves each feature a bee " +
			"proposes and merges each approved pull request.",
		Settings: settings(true, true, true, true, true, false, true, false),
	},
	{
		Name:    "slop-factory",
		Summary: "the full staff with auto-merge on and no approval gate on proposed features",
		When: "The full staff, and nothing waits for a person: the reviewer merges an " +
			"approved pull request as soon as its checks are green, and a feature the " +
			"product manager designs from QA's reports goes straight into the queue " +
			"without a person's approval. A person writes the first issues and " +
			"watches.",
		Settings: settings(true, true, true, true, true, true, false, false),
	},
	{
		Name:    "reviewer",
		Summary: "reviews pull requests other people open and builds nothing",
		When: "Nothing is built. The reviewer reviews the pull requests other people " +
			"open and submits each review on GitHub: every open pull request in the " +
			"filter that the factory did not write, which with filter.assignee set " +
			"means the ones assigned to the bee. filter.assignee is a login, so no " +
			"template sets it: set it with bees init --template reviewer --assignee " +
			"<login>.",
		Settings: settings(false, false, false, true, false, false, true, true),
	},
	{
		Name:    "planner",
		Summary: "the two managers scope and break down issues, nobody builds",
		When: "Only the two managers run: the product manager turns feature issues into " +
			"work items and the project manager triages them until they are ready to " +
			"build, and there it stops. No developer picks anything up, nothing is " +
			"reviewed and nothing is tested. Use it to have the backlog scoped before " +
			"deciding who builds it.",
		Settings: settings(true, true, false, false, false, false, true, false),
	},
}

// Templates returns every template in a stable order: contributor,
// issue-driven, slop-factory, reviewer, planner.
func Templates() []Template {
	return slices.Clone(templates)
}

// TemplateByName resolves a template name. An unknown name is an error that
// lists the names that exist.
func TemplateByName(name string) (Template, error) {
	for _, t := range templates {
		if t.Name == name {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("unknown template %q (want one of %s)", name, strings.Join(TemplateNames(), ", "))
}

// TemplateNames returns the template names in Templates order.
func TemplateNames() []string {
	names := make([]string, 0, len(templates))
	for _, t := range templates {
		names = append(names, t.Name)
	}
	return names
}

// setting is the template func behind every line a Template may activate:
// {{setting . "roles.qa.enabled" "#enabled = true"}} renders the commented
// line verbatim unless a template is selected and sets the key, in which case
// it renders the key's leaf name, "=", and the template's value. Nothing else
// in the rendered file depends on the template, which is what keeps the
// default render byte-for-byte the same.
func setting(o RenderOptions, key, commented string) string {
	if o.Template == nil {
		return commented
	}
	v, ok := o.Template.Settings[key]
	if !ok {
		return commented
	}
	leaf := key[strings.LastIndex(key, ".")+1:]
	return leaf + " = " + v
}

// header is the comment block RenderTOML writes above the file for a
// template: the name, then the When paragraph, wrapped at the width the rest
// of the file uses, so the file records why it is shaped the way it is.
func (t Template) header() string {
	var b strings.Builder
	b.WriteString("# Template: " + t.Name + "\n#\n")
	for _, line := range wrapWords(t.When, commentWidth-2) {
		b.WriteString("# " + line + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// commentWidth is the column the comments in beesTOMLTemplate wrap at.
const commentWidth = 80

// wrapWords greedily wraps text into lines of at most width characters,
// breaking only between words. A word longer than width gets a line of its
// own.
func wrapWords(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// Difference is one setting on which a config and a template disagree.
type Difference struct {
	Key      string // bees.toml key path, e.g. "roles.qa.enabled"
	Config   string // the resolved value, as a TOML literal
	Template string // what the template sets
}

// resolved maps each key a template may set to the value the factory runs
// with, read through the same accessors the scheduler uses. The comparison is
// on the resolved value, not on the file text: a key a person left commented
// out reads as its default, which is what the factory does. A key in
// TemplateKeys with no entry here is compared against nothing, which
// TestEveryTemplateKeyResolves rules out.
var resolved = map[string]func(*Config) string{
	"scheduler.review_assigned_prs": func(c *Config) string { return tomlBool(c.Scheduler.ReviewAssignedPRs) },
	"scheduler.feature_proposals":   func(c *Config) string { return tomlBool(c.Scheduler.Proposals()) },
	"roles.product_manager.enabled": func(c *Config) string { return tomlBool(roleEnabled(c, RoleProductManager)) },
	"roles.project_manager.enabled": func(c *Config) string { return tomlBool(roleEnabled(c, RoleProjectManager)) },
	"roles.developer.enabled":       func(c *Config) string { return tomlBool(roleEnabled(c, RoleDeveloper)) },
	"roles.reviewer.enabled":        func(c *Config) string { return tomlBool(roleEnabled(c, RoleReviewer)) },
	"roles.reviewer.auto_merge":     func(c *Config) string { return tomlBool(c.Merge().AutoMerge) },
	"roles.qa.enabled":              func(c *Config) string { return tomlBool(roleEnabled(c, RoleQA)) },
}

// roleEnabled resolves roles.<name>.enabled. Config.Role only fails on a name
// that is not a role, and every name here is one of the five constants.
func roleEnabled(c *Config, name string) bool {
	r, err := c.Role(name)
	if err != nil {
		return false
	}
	return r.Enabled
}

func tomlBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// Compare returns the settings on which c differs from t, in the order the
// template lists them. Empty means the factory runs the way the template
// describes. Only the keys the template sets are compared, so everything a
// template says nothing about — repo, filter, models, budgets, intervals — is
// ignored by construction.
func (t Template) Compare(c *Config) []Difference {
	var diffs []Difference
	for _, key := range templateKeys {
		want, sets := t.Settings[key]
		if !sets {
			continue
		}
		read, ok := resolved[key]
		if !ok {
			continue
		}
		if got := read(c); got != want {
			diffs = append(diffs, Difference{Key: key, Config: got, Template: want})
		}
	}
	return diffs
}

// Closest returns the template c differs from least, and those differences.
// Differing settings are counted, not weighted; ties go to the earlier
// template in Templates(). Every config has a closest template: there is no
// "matches nothing".
func Closest(c *Config) (Template, []Difference) {
	best, diffs := templates[0], templates[0].Compare(c)
	for _, t := range templates[1:] {
		if d := t.Compare(c); len(d) < len(diffs) {
			best, diffs = t, d
		}
	}
	return best, diffs
}
