package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeProvider struct {
	responses []GeneratedQuery
	calls     int
}

func (p *fakeProvider) Complete(_ context.Context, _ []ChatMessage) (GeneratedQuery, error) {
	if p.calls >= len(p.responses) {
		return GeneratedQuery{}, errors.New("no fake response available")
	}
	response := p.responses[p.calls]
	p.calls++
	return response, nil
}

func TestValidateSPARQLAddsLimit(t *testing.T) {
	query := `PREFIX rism: <https://rism.online/api/v1#>
SELECT ?source WHERE {
  ?source a rism:Source .
}`

	result := validateSPARQL(query, 25)
	if !result.Valid {
		t.Fatalf("expected query to be valid, got errors: %v", result.Errors)
	}
	if !strings.Contains(result.Query, "LIMIT 25") {
		t.Fatalf("expected LIMIT to be added, got %q", result.Query)
	}
}

func TestValidateSPARQLRejectsDestructiveQuery(t *testing.T) {
	result := validateSPARQL(`DELETE WHERE { ?s ?p ?o }`, 100)
	if result.Valid {
		t.Fatal("expected destructive query to be rejected")
	}
}

func TestParseLogLevel(t *testing.T) {
	if parseLogLevel("debug") != LevelDebug {
		t.Fatal("expected debug log level")
	}
	if parseLogLevel("warning") != LevelWarn {
		t.Fatal("expected warning log level")
	}
	if parseLogLevel("unknown") != LevelInfo {
		t.Fatal("expected unknown log level to default to info")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "  ", "reasoning") != "reasoning" {
		t.Fatal("expected first non-empty value")
	}
	if firstNonEmpty("", " ") != "" {
		t.Fatal("expected empty result")
	}
}

func TestParseGeneratedQueryStripsCodeFence(t *testing.T) {
	content := "```json\n{\"sparql\":\"SELECT * WHERE { ?s ?p ?o } LIMIT 1\",\"explanation\":\"test\",\"assumptions\":[],\"warnings\":[]}\n```"
	generated, err := parseGeneratedQuery(content, false)
	if err != nil {
		t.Fatalf("expected generated JSON to parse: %v", err)
	}
	if generated.SPARQL == "" || generated.Explanation != "test" {
		t.Fatalf("unexpected generated query: %#v", generated)
	}
}

func TestParseGeneratedQueryExtractsWrappedJSON(t *testing.T) {
	content := "Here is the query:\n{\"sparql\":\"SELECT * WHERE { ?s ?p ?o } LIMIT 1\",\"explanation\":\"test\",\"assumptions\":[],\"warnings\":[]}\nUse it carefully."
	generated, err := parseGeneratedQuery(content, false)
	if err != nil {
		t.Fatalf("expected wrapped generated JSON to parse: %v", err)
	}
	if generated.SPARQL == "" || generated.Explanation != "test" {
		t.Fatalf("unexpected generated query: %#v", generated)
	}
}

func TestParseGeneratedQueryAcceptsStringNotes(t *testing.T) {
	content := `{
	  "sparql": "SELECT * WHERE { ?s ?p ?o } LIMIT 1",
	  "explanation": "test",
	  "assumptions": "One assumption.",
	  "warnings": "One warning."
	}`
	generated, err := parseGeneratedQuery(content, false)
	if err != nil {
		t.Fatalf("expected string notes to parse: %v", err)
	}
	if len(generated.Assumptions) != 1 || generated.Assumptions[0] != "One assumption." {
		t.Fatalf("unexpected assumptions: %#v", generated.Assumptions)
	}
	if len(generated.Warnings) != 1 || generated.Warnings[0] != "One warning." {
		t.Fatalf("unexpected warnings: %#v", generated.Warnings)
	}
}

func TestParseGeneratedQueryDebugErrorIncludesSnippet(t *testing.T) {
	_, err := parseGeneratedQuery("not json at all", true)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "raw content snippet") {
		t.Fatalf("expected debug error to include raw snippet, got %q", err.Error())
	}
}

func TestGenerateValidatedQueryRepairsRejectedQuery(t *testing.T) {
	provider := &fakeProvider{
		responses: []GeneratedQuery{
			{
				SPARQL:      "DELETE WHERE { ?s ?p ?o }",
				Explanation: "bad",
			},
			{
				SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . }",
				Explanation: "fixed",
			},
		},
	}
	server := &Server{
		config: Config{
			MaxRepairRetries: 2,
			DefaultLimit:     10,
		},
		provider:       provider,
		promptTemplate: "Generate Linked RISM SPARQL and return JSON.",
	}

	response, err := server.generateValidatedQuery(context.Background(), "List sources")
	if err != nil {
		t.Fatalf("expected generation to succeed: %v", err)
	}
	if !response.Valid {
		t.Fatalf("expected repaired query to be valid: %#v", response)
	}
	if response.Attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", response.Attempts)
	}
	if !strings.Contains(response.SPARQL, "LIMIT 10") {
		t.Fatalf("expected repaired query to include default limit, got %q", response.SPARQL)
	}
}
