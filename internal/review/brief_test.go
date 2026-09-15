package review

func testBrief() *Brief {
	return &Brief{
		Ref:                Ref{Repo: testRepo, Number: 7},
		Title:              "widgets: gather the context",
		Author:             "octocat",
		Summary:            "gathers the context sources a project declares",
		Size:               "m",
		AcceptanceCriteria: []Point{{Text: "a source that cannot read something does not fail the review", Source: "#566"}, {Text: "nothing is truncated"}},
		StyleRules:         []Point{{Text: "every new key needs a test", Source: "CLAUDE.md"}},
		TouchedAreas:       []TouchedArea{{Name: "internal/review", Paths: []string{"gather.go", "sources.go"}, Summary: "the pipeline and its sources"}},
		Sources:            []string{SourceDiff, SourceStyleFiles},
		NotGathered:        []string{"#99 is not an issue that could be read"},
		SessionID:          "sess-1",
	}
}
