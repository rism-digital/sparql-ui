package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type fakeProvider struct {
	responses []LLMCompletion
	calls     int
}

func (p *fakeProvider) Complete(_ context.Context, _ []ChatMessage, _ []ToolDefinition) (LLMCompletion, error) {
	if p.calls >= len(p.responses) {
		return LLMCompletion{}, errors.New("no fake response available")
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

func TestResolveConfiguredPathFallsBackToExistingCandidate(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/prompt.md"
	if err := os.WriteFile(file, []byte("prompt"), 0o644); err != nil {
		t.Fatalf("expected temp prompt to be written: %v", err)
	}
	resolved := resolveConfiguredPath("missing.md", file)
	if resolved != file {
		t.Fatalf("expected fallback path %q, got %q", file, resolved)
	}
}

func TestBuildOntologyPromptExtractsQueryPatterns(t *testing.T) {
	content := `rism:ManuscriptSource
    a owl:Class ;
    rdfs:label "Manuscript source"@en ;
    rdfs:comment "A source type for manuscripts."@en ;
    rism:queryPattern "?source rism:sourceTypes/rism:sourceType ?sourceType . ?sourceType a rism:ManuscriptSource ." ;
    .`

	prompt := buildOntologyPrompt(content)
	if !strings.Contains(prompt, "Manuscript source") {
		t.Fatalf("expected ontology label in prompt, got %q", prompt)
	}
	if !strings.Contains(prompt, "?source rism:sourceTypes/rism:sourceType ?sourceType") {
		t.Fatalf("expected query pattern in prompt, got %q", prompt)
	}
}

func TestBuildMessagesIncludesOntologyPrompt(t *testing.T) {
	server := &Server{
		promptTemplate: "Base prompt.",
		ontologyPrompt: "Ontology prompt.",
	}

	messages := server.buildMessages("List manuscripts", GeneratedQuery{}, nil, false)
	if len(messages) != 2 {
		t.Fatalf("expected system and user messages, got %#v", messages)
	}
	system, ok := messages[0].Content.(string)
	if !ok {
		t.Fatalf("expected string system prompt, got %#v", messages[0].Content)
	}
	if !strings.Contains(system, "Base prompt.") || !strings.Contains(system, "Ontology prompt.") {
		t.Fatalf("expected ontology prompt to be appended, got %q", system)
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
		responses: []LLMCompletion{
			{
				Generated: &GeneratedQuery{
					SPARQL:      "DELETE WHERE { ?s ?p ?o }",
					Explanation: "bad",
				},
			},
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . }",
					Explanation: "fixed",
				},
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

	response, err := server.generateValidatedQuery(context.Background(), "List sources", nil)
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

func TestGenerateValidatedQueryRetriesEmptySPARQL(t *testing.T) {
	provider := &fakeProvider{
		responses: []LLMCompletion{
			{
				Generated: &GeneratedQuery{
					SPARQL:      "",
					Explanation: "I need to inspect examples first.",
				},
			},
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . } LIMIT 10",
					Explanation: "fixed",
				},
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

	response, err := server.generateValidatedQuery(context.Background(), "List sources", nil)
	if err != nil {
		t.Fatalf("expected generation to recover from empty SPARQL: %v", err)
	}
	if !response.Valid || response.Attempts != 2 {
		t.Fatalf("expected valid response on second attempt, got %#v", response)
	}
}

func TestGenerateValidatedQueryRetriesPlanningPlaceholder(t *testing.T) {
	provider := &fakeProvider{
		responses: []LLMCompletion{
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?source ?title ?composer ?siglum WHERE { ?source a <https://rism.online/api/v1#Source> . } LIMIT 1",
					Explanation: "I need to verify the RDF paths before writing the SPARQL query.",
					Warnings:    []string{"Query is a placeholder while I inspect the RDF patterns."},
				},
			},
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?source ?title WHERE { ?source a <https://rism.online/api/v1#Source> ; <http://www.w3.org/2000/01/rdf-schema#label> ?title . } LIMIT 10",
					Explanation: "fixed",
				},
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

	response, err := server.generateValidatedQuery(context.Background(), "List sources", nil)
	if err != nil {
		t.Fatalf("expected generation to recover from placeholder: %v", err)
	}
	if !response.Valid || response.Attempts != 2 {
		t.Fatalf("expected valid response on second attempt, got %#v", response)
	}
}

func TestGenerateValidatedQueryEmitsProgressEvents(t *testing.T) {
	provider := &fakeProvider{
		responses: []LLMCompletion{
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . }",
					Explanation: "lists sources",
				},
			},
		},
	}
	server := &Server{
		config: Config{
			MaxRepairRetries: 1,
			DefaultLimit:     10,
		},
		provider:       provider,
		promptTemplate: "Generate Linked RISM SPARQL and return JSON.",
	}
	var events []ProgressEvent
	emit := func(event ProgressEvent) {
		events = append(events, event)
	}

	response, err := server.generateValidatedQuery(context.Background(), "List sources", emit)
	if err != nil {
		t.Fatalf("expected generation to succeed: %v", err)
	}
	if !response.Valid {
		t.Fatalf("expected valid response, got %#v", response)
	}
	if len(events) == 0 {
		t.Fatal("expected progress events")
	}
	if !hasProgressEvent(events, "llm", "active") {
		t.Fatalf("expected active llm event, got %#v", events)
	}
	if !hasProgressEvent(events, "validation", "done") {
		t.Fatalf("expected completed validation event, got %#v", events)
	}
}

func TestGenerateValidatedQueryEmitsRetryForRecoverableValidationFailure(t *testing.T) {
	provider := &fakeProvider{
		responses: []LLMCompletion{
			{
				Generated: &GeneratedQuery{
					SPARQL:      "DELETE WHERE { ?s ?p ?o }",
					Explanation: "bad",
				},
			},
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . } LIMIT 10",
					Explanation: "fixed",
				},
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
	var events []ProgressEvent
	emit := func(event ProgressEvent) {
		events = append(events, event)
	}

	response, err := server.generateValidatedQuery(context.Background(), "List sources", emit)
	if err != nil {
		t.Fatalf("expected generation to succeed: %v", err)
	}
	if !response.Valid {
		t.Fatalf("expected valid response, got %#v", response)
	}
	if !hasProgressEvent(events, "validation", "retry") {
		t.Fatalf("expected retry validation event, got %#v", events)
	}
}

func TestProgressBrokerSubscribeEmitUnsubscribe(t *testing.T) {
	broker := newProgressBroker()
	events := broker.subscribe("request-1")
	broker.emit(ProgressEvent{RequestID: "request-1", Type: "progress", Step: "llm", State: "active"})

	select {
	case event := <-events:
		if event.Step != "llm" {
			t.Fatalf("unexpected event: %#v", event)
		}
	default:
		t.Fatal("expected progress event")
	}

	broker.unsubscribe("request-1", events)
	broker.emit(ProgressEvent{RequestID: "request-1", Type: "progress", Step: "validation", State: "done"})
	select {
	case event := <-events:
		t.Fatalf("did not expect event after unsubscribe: %#v", event)
	default:
	}
}

func hasProgressEvent(events []ProgressEvent, step, state string) bool {
	for _, event := range events {
		if event.Step == step && event.State == state {
			return true
		}
	}
	return false
}

func TestIsPlanningPlaceholder(t *testing.T) {
	if !isPlanningPlaceholder(GeneratedQuery{
		SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . } LIMIT 1",
		Explanation: "I will first inspect the RDF examples before writing the query.",
	}) {
		t.Fatal("expected planning language to be detected")
	}
	if isPlanningPlaceholder(GeneratedQuery{
		SPARQL:      "SELECT ?source WHERE { ?source a <https://rism.online/api/v1#Source> . } LIMIT 1",
		Explanation: "Returns one source.",
	}) {
		t.Fatal("did not expect simple final query to be rejected without planning language")
	}
}

func TestToolLoopExecutesValidateTool(t *testing.T) {
	provider := &fakeProvider{
		responses: []LLMCompletion{
			{
				ToolCalls: []ToolCall{
					{
						ID:   "call-1",
						Type: "function",
						Function: ToolCallFunction{
							Name:      "validate_sparql",
							Arguments: `{"query":"SELECT ?s WHERE { ?s ?p ?o }"}`,
						},
					},
				},
			},
			{
				Generated: &GeneratedQuery{
					SPARQL:      "SELECT ?s WHERE { ?s ?p ?o } LIMIT 10",
					Explanation: "used validation",
				},
			},
		},
	}
	server := &Server{
		config: Config{
			MaxToolIterations: 2,
			DefaultLimit:      10,
		},
		provider:       provider,
		promptTemplate: "Generate Linked RISM SPARQL and return JSON.",
	}

	generated, err := server.runToolLoop(context.Background(), server.buildMessages("test", GeneratedQuery{}, nil, false), nil, 1)
	if err != nil {
		t.Fatalf("expected tool loop to succeed: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("expected two provider calls, got %d", provider.calls)
	}
	if len(generated.ToolTrace) == 0 || !strings.Contains(generated.ToolTrace[0], "validate_sparql") {
		t.Fatalf("expected validate_sparql trace, got %#v", generated.ToolTrace)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	if _, err := safeJoin("rdf-examples", "../.env"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}
