package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	defaultAddr           = "127.0.0.1:8787"
	defaultModel          = "moonshotai/Kimi-K2.6"
	defaultSPARQLEndpoint = "https://linked.rism.io/api"
	defaultPromptPath     = "prompts/rism-nl2sparql.md"
	defaultRDFExamplesDir = "rdf-examples"
	defaultOntologyPath   = "rdf-examples/rism-service-ontology.ttl"
	defaultRGPath         = "rg"
	defaultMaxRetries     = 2
	defaultQueryLimit     = 100
	defaultMaxToolIters   = 4
	defaultDiscoveryTTL   = 3600
	maxInstructionLength  = 4000
	maxQueryLength        = 20000
	maxToolResultBytes    = 12000
)

type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	destructiveQueryPattern = regexp.MustCompile(`(?i)\b(INSERT|DELETE|LOAD|CLEAR|CREATE|DROP|MOVE|COPY|ADD|WITH)\b`)
	queryTypePattern        = regexp.MustCompile(`(?is)^\s*(?:PREFIX\s+[A-Za-z][\w-]*:\s*<[^>]+>\s*)*(SELECT|ASK|CONSTRUCT|DESCRIBE)\b`)
	limitPattern            = regexp.MustCompile(`(?i)\bLIMIT\s+\d+\b`)
	codeFencePattern        = regexp.MustCompile("(?is)^\\s*```(?:json|sparql)?\\s*(.*?)\\s*```\\s*$")
	requestIDPattern        = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	currentLogLevel         = LevelInfo
)

type Config struct {
	Addr                      string
	InfomaniakAPIToken        string
	InfomaniakProductID       string
	InfomaniakModel           string
	InfomaniakReasoningEffort string
	SPARQLEndpoint            string
	PromptTemplatePath        string
	PromptCacheKey            string
	RDFExamplesDir            string
	OntologyPath              string
	RGPath                    string
	EnableEndpointValidation  bool
	DebugLLM                  bool
	LogLevel                  LogLevel
	MaxRepairRetries          int
	MaxToolIterations         int
	DefaultLimit              int
	DiscoveryCacheTTL         time.Duration
}

type Server struct {
	config         Config
	client         *http.Client
	provider       LLMProvider
	promptTemplate string
	ontologyPrompt string
	cacheMu        sync.Mutex
	discoveryCache map[string]cacheEntry
	progress       *progressBroker
}

type LLMProvider interface {
	Complete(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (LLMCompletion, error)
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDefinition struct {
	Type     string          `json:"type"`
	Function ToolFunctionDef `json:"function"`
}

type ToolFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type LLMCompletion struct {
	Generated *GeneratedQuery
	ToolCalls []ToolCall
}

type GenerateRequest struct {
	RequestID    string `json:"request_id,omitempty"`
	Instructions string `json:"instructions"`
}

type ProgressEvent struct {
	Type      string    `json:"type"`
	RequestID string    `json:"request_id"`
	Step      string    `json:"step,omitempty"`
	State     string    `json:"state,omitempty"`
	Message   string    `json:"message"`
	Attempt   int       `json:"attempt,omitempty"`
	Iteration int       `json:"iteration,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type progressEmitter func(ProgressEvent)

type progressBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan ProgressEvent]struct{}
}

type GeneratedQuery struct {
	SPARQL      string   `json:"sparql"`
	Explanation string   `json:"explanation"`
	Assumptions []string `json:"assumptions"`
	Warnings    []string `json:"warnings"`
	ToolTrace   []string `json:"tool_trace"`
}

type rawGeneratedQuery struct {
	SPARQL      string          `json:"sparql"`
	Explanation string          `json:"explanation"`
	Assumptions json.RawMessage `json:"assumptions"`
	Warnings    json.RawMessage `json:"warnings"`
	ToolTrace   json.RawMessage `json:"tool_trace"`
}

type GenerateResponse struct {
	SPARQL      string   `json:"sparql"`
	Explanation string   `json:"explanation"`
	Assumptions []string `json:"assumptions"`
	Warnings    []string `json:"warnings"`
	ToolTrace   []string `json:"tool_trace"`
	Valid       bool     `json:"valid"`
	Attempts    int      `json:"attempts"`
	Errors      []string `json:"errors,omitempty"`
}

type cacheEntry struct {
	Value     string
	ExpiresAt time.Time
}

type ValidationResult struct {
	Query  string
	Valid  bool
	Errors []string
}

type InfomaniakProvider struct {
	config   Config
	client   *http.Client
	debugLLM bool
}

type LLMParseError struct {
	Message     string
	RawSnippet  string
	ContentSize int
	Kind        string
}

func (e LLMParseError) Error() string {
	if e.RawSnippet == "" {
		return e.Message
	}
	return fmt.Sprintf("%s; raw content size=%d; raw content snippet=%q", e.Message, e.ContentSize, e.RawSnippet)
}

func isEmptySPARQLError(err error) bool {
	var parseErr LLMParseError
	return errors.As(err, &parseErr) && parseErr.Kind == "empty_sparql"
}

type infomaniakRequest struct {
	Model               string           `json:"model"`
	Messages            []ChatMessage    `json:"messages"`
	Temperature         float64          `json:"temperature"`
	MaxCompletionTokens int              `json:"max_completion_tokens"`
	Stream              bool             `json:"stream"`
	PromptCacheKey      string           `json:"prompt_cache_key,omitempty"`
	ReasoningEffort     string           `json:"reasoning_effort,omitempty"`
	ResponseFormat      map[string]any   `json:"response_format,omitempty"`
	ParallelToolCalls   bool             `json:"parallel_tool_calls"`
	Tools               []ToolDefinition `json:"tools,omitempty"`
	ToolChoice          string           `json:"tool_choice,omitempty"`
}

type infomaniakResponse struct {
	Choices []struct {
		Message struct {
			Content          *string    `json:"content"`
			Reasoning        string     `json:"reasoning"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

func main() {
	if dotEnvPath := findExistingPath(".env", "../.env"); dotEnvPath != "" {
		if err := loadDotEnv(dotEnvPath); err != nil {
			log.Printf("Skipping .env load: %s", err)
		}
	}
	config := loadConfig()
	currentLogLevel = config.LogLevel
	if config.InfomaniakAPIToken == "" || config.InfomaniakProductID == "" {
		logWarn("INFOMANIAK_API_TOKEN and INFOMANIAK_PRODUCT_ID are required for /api/generate-sparql")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	promptTemplate, err := loadPromptTemplate(config.PromptTemplatePath)
	if err != nil {
		log.Fatal(err)
	}
	ontologyPrompt, err := loadOntologyPrompt(config.OntologyPath)
	if err != nil {
		logWarn("Skipping ontology prompt load: %s", err)
	}
	server := &Server{
		config:         config,
		client:         client,
		promptTemplate: promptTemplate,
		ontologyPrompt: ontologyPrompt,
		discoveryCache: make(map[string]cacheEntry),
		progress:       newProgressBroker(),
	}
	server.provider = &InfomaniakProvider{config: config, client: client, debugLLM: config.DebugLLM}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealth)
	mux.HandleFunc("/api/generate-sparql", server.handleGenerateSPARQL)
	mux.HandleFunc("/api/generate-sparql/events", server.handleGenerateEvents)

	httpServer := &http.Server{
		Addr:              config.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logInfo("Linked RISM NL-to-SPARQL API listening on http://%s", config.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("%s:%d: missing key", path, lineNumber)
		}
		value = strings.Trim(value, `"'`)

		if os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func loadConfig() Config {
	return Config{
		Addr:                      envOrDefault("NL2SPARQL_ADDR", defaultAddr),
		InfomaniakAPIToken:        os.Getenv("INFOMANIAK_API_TOKEN"),
		InfomaniakProductID:       os.Getenv("INFOMANIAK_PRODUCT_ID"),
		InfomaniakModel:           envOrDefault("INFOMANIAK_MODEL", defaultModel),
		InfomaniakReasoningEffort: envOrDefault("INFOMANIAK_REASONING_EFFORT", "none"),
		SPARQLEndpoint:            envOrDefault("SPARQL_ENDPOINT", defaultSPARQLEndpoint),
		PromptTemplatePath:        resolveConfiguredPath(envOrDefault("PROMPT_TEMPLATE_PATH", defaultPromptPath), "../"+defaultPromptPath),
		PromptCacheKey:            envOrDefault("PROMPT_CACHE_KEY", "linked-rism-nl2sparql-v1"),
		RDFExamplesDir:            resolveConfiguredPath(envOrDefault("RDF_EXAMPLES_DIR", defaultRDFExamplesDir), "../"+defaultRDFExamplesDir),
		OntologyPath:              resolveConfiguredPath(envOrDefault("ONTOLOGY_PATH", defaultOntologyPath), "../"+defaultOntologyPath),
		RGPath:                    envOrDefault("RG_PATH", defaultRGPath),
		EnableEndpointValidation:  strings.EqualFold(os.Getenv("ENABLE_ENDPOINT_VALIDATION"), "true"),
		DebugLLM:                  strings.EqualFold(os.Getenv("DEBUG_LLM"), "true"),
		LogLevel:                  parseLogLevel(envOrDefault("LOG_LEVEL", "info")),
		MaxRepairRetries:          intEnvOrDefault("MAX_REPAIR_RETRIES", defaultMaxRetries),
		MaxToolIterations:         intEnvOrDefault("MAX_TOOL_ITERATIONS", defaultMaxToolIters),
		DefaultLimit:              intEnvOrDefault("DEFAULT_QUERY_LIMIT", defaultQueryLimit),
		DiscoveryCacheTTL:         time.Duration(intEnvOrDefault("DISCOVERY_CACHE_TTL_SECONDS", defaultDiscoveryTTL)) * time.Second,
	}
}

func resolveConfiguredPath(primary string, fallback string) string {
	resolved := findExistingPath(primary, fallback)
	if resolved != "" {
		return resolved
	}
	return primary
}

func findExistingPath(candidates ...string) string {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func envOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func parseLogLevel(value string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func logDebug(format string, args ...any) {
	logAt(LevelDebug, "DEBUG", format, args...)
}

func logInfo(format string, args ...any) {
	logAt(LevelInfo, "INFO", format, args...)
}

func logWarn(format string, args ...any) {
	logAt(LevelWarn, "WARN", format, args...)
}

func logError(format string, args ...any) {
	logAt(LevelError, "ERROR", format, args...)
}

func logAt(level LogLevel, label string, format string, args ...any) {
	if level < currentLogLevel {
		return
	}
	log.Printf("%s: %s", label, fmt.Sprintf(format, args...))
}

func newProgressBroker() *progressBroker {
	return &progressBroker{subscribers: make(map[string]map[chan ProgressEvent]struct{})}
}

func (b *progressBroker) subscribe(requestID string) chan ProgressEvent {
	ch := make(chan ProgressEvent, 64)
	b.mu.Lock()
	if b.subscribers[requestID] == nil {
		b.subscribers[requestID] = make(map[chan ProgressEvent]struct{})
	}
	b.subscribers[requestID][ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *progressBroker) unsubscribe(requestID string, ch chan ProgressEvent) {
	b.mu.Lock()
	if subscribers, ok := b.subscribers[requestID]; ok {
		delete(subscribers, ch)
		if len(subscribers) == 0 {
			delete(b.subscribers, requestID)
		}
	}
	b.mu.Unlock()
}

func (b *progressBroker) emit(event ProgressEvent) {
	b.mu.Lock()
	subscribers := b.subscribers[event.RequestID]
	for ch := range subscribers {
		select {
		case ch <- event:
		default:
			logDebug("progress subscriber channel full: request_id=%s", event.RequestID)
		}
	}
	b.mu.Unlock()
}

func loadPromptTemplate(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read prompt template %q: %w", path, err)
	}
	template := strings.TrimSpace(string(content))
	if template == "" {
		return "", fmt.Errorf("prompt template %q is empty", path)
	}
	return template, nil
}

func loadOntologyPrompt(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read ontology %q: %w", path, err)
	}
	summary := buildOntologyPrompt(string(content))
	if summary == "" {
		return "", fmt.Errorf("ontology %q did not contain usable labels or query patterns", path)
	}
	return summary, nil
}

func buildOntologyPrompt(content string) string {
	terms, patterns := extractOntologyTermsAndPatterns(content)
	var builder strings.Builder
	builder.WriteString("RISM service ontology reference:\n")
	builder.WriteString("- Prefer ontology query patterns over guessing RDF paths.\n")
	builder.WriteString("- Use the search_rism_ontology tool for more detail when a request mentions a class, predicate, source type, holding, incipit, relationship, external resource, or summary field not already covered here.\n")
	if len(patterns) > 0 {
		builder.WriteString("\nOntology query patterns:\n")
		for _, pattern := range patterns {
			builder.WriteString("- ")
			builder.WriteString(pattern)
			builder.WriteString("\n")
		}
	}
	if len(terms) > 0 {
		builder.WriteString("\nOntology terms:\n")
		for _, term := range terms {
			builder.WriteString("- ")
			builder.WriteString(term)
			builder.WriteString("\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

func extractOntologyTermsAndPatterns(content string) ([]string, []string) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var terms []string
	var patterns []string
	currentSubject := ""
	currentLabel := ""
	currentComment := ""

	flush := func() {
		if currentSubject != "" && currentLabel != "" && len(terms) < 80 {
			term := currentSubject + " - " + currentLabel
			if currentComment != "" {
				term += ": " + currentComment
			}
			terms = append(terms, term)
		}
		currentSubject = ""
		currentLabel = ""
		currentComment = ""
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, ".") {
			flush()
			continue
		}
		if isOntologySubjectLine(line) {
			flush()
			currentSubject = strings.TrimSuffix(line, ";")
			continue
		}
		if strings.HasPrefix(line, "rdfs:label ") && currentLabel == "" {
			currentLabel = extractTTLQuotedValue(line)
			continue
		}
		if strings.HasPrefix(line, "rdfs:comment ") && currentComment == "" {
			currentComment = extractTTLQuotedValue(line)
			continue
		}
		if strings.HasPrefix(line, "rism:queryPattern ") && len(patterns) < 120 {
			if pattern := extractTTLQuotedValue(line); pattern != "" {
				patterns = append(patterns, pattern)
			}
		}
	}
	flush()
	return terms, patterns
}

func isOntologySubjectLine(line string) bool {
	if strings.Contains(line, " ") || strings.Contains(line, "\t") {
		return false
	}
	return strings.HasPrefix(line, "rism:") || strings.HasPrefix(line, "dcterms:") || strings.HasPrefix(line, "rdfs:") || strings.HasPrefix(line, "schemaorg:") || strings.HasPrefix(line, "pmo:")
}

func extractTTLQuotedValue(line string) string {
	start := strings.Index(line, `"`)
	if start < 0 {
		return ""
	}
	escaped := false
	for i := start + 1; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			value := line[start+1 : i]
			value = strings.ReplaceAll(value, `\"`, `"`)
			value = strings.ReplaceAll(value, `\\`, `\`)
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func intEnvOrDefault(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeCORSHeaders(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGenerateSPARQL(w http.ResponseWriter, r *http.Request) {
	s.writeCORSHeaders(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var request GenerateRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	instructions := strings.TrimSpace(request.Instructions)
	if instructions == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instructions are required"})
		return
	}
	if len(instructions) > maxInstructionLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instructions are too long"})
		return
	}
	requestID := strings.TrimSpace(request.RequestID)
	if requestID != "" && !requestIDPattern.MatchString(requestID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request_id must be 1-128 URL-safe characters"})
		return
	}
	if s.config.InfomaniakAPIToken == "" || s.config.InfomaniakProductID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Infomaniak API configuration is missing"})
		return
	}

	logDebug("generate request received: instruction_length=%d", len(instructions))
	emit := s.progressEmitter(requestID)
	emit(ProgressEvent{Type: "progress", Step: "request", State: "done", Message: "Generation request accepted"})
	response, err := s.generateValidatedQuery(r.Context(), instructions, emit)
	if err != nil {
		logError("generate request failed: %s", err)
		emit(ProgressEvent{Type: "error", Step: "request", State: "error", Message: err.Error()})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	logDebug("generate request completed: valid=%t attempts=%d sparql_length=%d errors=%d", response.Valid, response.Attempts, len(response.SPARQL), len(response.Errors))
	if response.Valid {
		emit(ProgressEvent{Type: "complete", Step: "validation", State: "done", Message: fmt.Sprintf("Validated after %d attempt(s)", response.Attempts), Attempt: response.Attempts})
	} else {
		emit(ProgressEvent{Type: "error", Step: "validation", State: "error", Message: fmt.Sprintf("Generation failed after %d attempt(s): %s", response.Attempts, strings.Join(response.Errors, "; ")), Attempt: response.Attempts})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGenerateEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	requestID := strings.TrimSpace(r.URL.Query().Get("request_id"))
	if !requestIDPattern.MatchString(requestID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request_id is required"})
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && !isAllowedOrigin(origin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logDebug("websocket accept failed: %s", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	events := s.progress.subscribe(requestID)
	defer s.progress.unsubscribe(requestID, events)

	logDebug("progress websocket subscribed: request_id=%s", requestID)
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			payload, err := json.Marshal(event)
			if err != nil {
				logDebug("progress event marshal failed: %s", err)
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			err = conn.Write(ctx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				logDebug("progress websocket write failed: request_id=%s error=%s", requestID, err)
				return
			}
			if event.Type == "complete" || event.Type == "error" {
				return
			}
		}
	}
}

func (s *Server) progressEmitter(requestID string) progressEmitter {
	if requestID == "" || s.progress == nil {
		return func(ProgressEvent) {}
	}
	return func(event ProgressEvent) {
		event.RequestID = requestID
		event.Timestamp = time.Now().UTC()
		if event.Type == "" {
			event.Type = "progress"
		}
		s.progress.emit(event)
	}
}

func (s *Server) writeCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	}
}

func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	if origin == "null" {
		return true
	}
	host, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://"))
	if err != nil {
		return origin == "http://localhost" || origin == "http://127.0.0.1"
	}
	return host == "localhost" || host == "127.0.0.1"
}

func decodeJSONBody(r *http.Request, target any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("request body is empty")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("request body must be valid JSON")
	}
	return nil
}

func (s *Server) generateValidatedQuery(ctx context.Context, instructions string, emit progressEmitter) (GenerateResponse, error) {
	if emit == nil {
		emit = func(ProgressEvent) {}
	}
	var validation ValidationResult
	var generated GeneratedQuery
	var err error
	attempts := 0

	for attempt := 0; attempt <= s.config.MaxRepairRetries; attempt++ {
		attempts = attempt + 1
		hasRetryRemaining := attempt < s.config.MaxRepairRetries
		logDebug("generation attempt %d started: repair=%t", attempts, attempt > 0)
		emit(ProgressEvent{Type: "progress", Step: "llm", State: "active", Message: fmt.Sprintf("Starting generation attempt %d", attempts), Attempt: attempts})
		messages := s.buildMessages(instructions, generated, validation.Errors, attempt > 0)
		generated, err = s.runToolLoop(ctx, messages, emit, attempts)
		if err != nil {
			if isEmptySPARQLError(err) {
				validation = invalidResult("", err.Error())
				logDebug("generation attempt %d returned empty SPARQL final answer; retrying as validation failure", attempts)
				emit(ProgressEvent{Type: "progress", Step: "llm", State: retryState(hasRetryRemaining), Message: retryMessage("LLM returned an empty SPARQL field", hasRetryRemaining), Attempt: attempts})
				continue
			}
			logDebug("generation attempt %d provider error: %s", attempts, err)
			emit(ProgressEvent{Type: "error", Step: "llm", State: "error", Message: err.Error(), Attempt: attempts})
			return GenerateResponse{}, err
		}
		logDebug("generation attempt %d returned query: sparql_length=%d explanation_length=%d", attempts, len(generated.SPARQL), len(generated.Explanation))
		emit(ProgressEvent{Type: "progress", Step: "llm", State: "done", Message: "Draft query received", Attempt: attempts})

		if isPlanningPlaceholder(generated) {
			validation = invalidResult(generated.SPARQL, "LLM returned a planning or placeholder query instead of a final query; call tools if more information is needed, otherwise return the complete SPARQL query for the user request")
			logDebug("generation attempt %d returned planning placeholder; retrying as validation failure", attempts)
			emit(ProgressEvent{Type: "progress", Step: "validation", State: retryState(hasRetryRemaining), Message: retryMessage("Placeholder query rejected", hasRetryRemaining), Attempt: attempts})
			continue
		}

		emit(ProgressEvent{Type: "progress", Step: "validation", State: "active", Message: "Running deterministic validation", Attempt: attempts})
		validation = validateSPARQL(generated.SPARQL, s.config.DefaultLimit)
		if validation.Valid && s.config.EnableEndpointValidation {
			logDebug("generation attempt %d running endpoint validation", attempts)
			emit(ProgressEvent{Type: "progress", Step: "validation", State: "active", Message: "Running endpoint validation", Attempt: attempts})
			validation = s.validateAgainstEndpoint(ctx, validation.Query)
		}
		if validation.Valid {
			logDebug("generation attempt %d validation succeeded", attempts)
			emit(ProgressEvent{Type: "progress", Step: "validation", State: "done", Message: "Query validation succeeded", Attempt: attempts})
			generated.SPARQL = validation.Query
			return GenerateResponse{
				SPARQL:      generated.SPARQL,
				Explanation: generated.Explanation,
				Assumptions: generated.Assumptions,
				Warnings:    generated.Warnings,
				ToolTrace:   generated.ToolTrace,
				Valid:       true,
				Attempts:    attempts,
			}, nil
		}
		logDebug("generation attempt %d validation failed: %s", attempts, strings.Join(validation.Errors, "; "))
		emit(ProgressEvent{Type: "progress", Step: "validation", State: retryState(hasRetryRemaining), Message: retryMessage(fmt.Sprintf("Server validation rejected the query: %s", strings.Join(validation.Errors, "; ")), hasRetryRemaining), Attempt: attempts})
	}

	return GenerateResponse{
		SPARQL:      generated.SPARQL,
		Explanation: generated.Explanation,
		Assumptions: generated.Assumptions,
		Warnings:    generated.Warnings,
		ToolTrace:   generated.ToolTrace,
		Valid:       false,
		Attempts:    attempts,
		Errors:      validation.Errors,
	}, nil
}

func (s *Server) runToolLoop(ctx context.Context, messages []ChatMessage, emit progressEmitter, attempt int) (GeneratedQuery, error) {
	if emit == nil {
		emit = func(ProgressEvent) {}
	}
	tools := s.toolDefinitions()
	toolTrace := []string{}

	for iteration := 0; iteration <= s.config.MaxToolIterations; iteration++ {
		emit(ProgressEvent{Type: "progress", Step: "llm", State: "active", Message: "Calling LLM provider", Attempt: attempt, Iteration: iteration + 1})
		completion, err := s.provider.Complete(ctx, messages, tools)
		if err != nil {
			emit(ProgressEvent{Type: "progress", Step: "llm", State: "error", Message: err.Error(), Attempt: attempt, Iteration: iteration + 1})
			return GeneratedQuery{}, err
		}
		if len(completion.ToolCalls) == 0 {
			if completion.Generated == nil {
				emit(ProgressEvent{Type: "progress", Step: "llm", State: "error", Message: "LLM response included neither a final query nor tool calls", Attempt: attempt, Iteration: iteration + 1})
				return GeneratedQuery{}, errors.New("LLM response included neither a final query nor tool calls")
			}
			if len(completion.Generated.ToolTrace) == 0 {
				completion.Generated.ToolTrace = toolTrace
			} else {
				completion.Generated.ToolTrace = append(toolTrace, completion.Generated.ToolTrace...)
			}
			emit(ProgressEvent{Type: "progress", Step: "llm", State: "done", Message: "LLM returned a final query draft", Attempt: attempt, Iteration: iteration + 1})
			return *completion.Generated, nil
		}
		if iteration == s.config.MaxToolIterations {
			emit(ProgressEvent{Type: "progress", Step: "tool", State: "error", Message: fmt.Sprintf("Exceeded max tool iterations (%d)", s.config.MaxToolIterations), Attempt: attempt, Iteration: iteration + 1})
			return GeneratedQuery{}, fmt.Errorf("LLM exceeded max tool iterations (%d)", s.config.MaxToolIterations)
		}

		logDebug("tool iteration %d: executing %d tool call(s)", iteration+1, len(completion.ToolCalls))
		emit(ProgressEvent{Type: "progress", Step: "tool", State: "active", Message: fmt.Sprintf("LLM requested %d tool call(s)", len(completion.ToolCalls)), Attempt: attempt, Iteration: iteration + 1})
		messages = append(messages, ChatMessage{
			Role:      "assistant",
			Content:   "",
			ToolCalls: completion.ToolCalls,
		})
		for _, call := range completion.ToolCalls {
			emit(ProgressEvent{Type: "progress", Step: "tool", State: "active", Message: fmt.Sprintf("Running %s", call.Function.Name), Attempt: attempt, Iteration: iteration + 1, Tool: call.Function.Name})
			result := s.executeToolCall(ctx, call)
			toolState := "done"
			if !result.OK {
				toolState = "error"
			}
			emit(ProgressEvent{Type: "progress", Step: "tool", State: toolState, Message: result.Trace(), Attempt: attempt, Iteration: iteration + 1, Tool: call.Function.Name})
			toolTrace = append(toolTrace, result.Trace())
			content, err := json.Marshal(result)
			if err != nil {
				return GeneratedQuery{}, err
			}
			messages = append(messages, ChatMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    string(content),
			})
		}
	}

	return GeneratedQuery{}, fmt.Errorf("LLM exceeded max tool iterations (%d)", s.config.MaxToolIterations)
}

func (s *Server) buildMessages(instructions string, previous GeneratedQuery, errorsList []string, repair bool) []ChatMessage {
	user := fmt.Sprintf("User request:\n%s", instructions)
	if repair {
		user = fmt.Sprintf(
			"The previous generated query was rejected by deterministic server-side validation.\n\nPrevious SPARQL:\n%s\n\nValidation errors:\n- %s\n\nOriginal user request:\n%s\n\nRegenerate a corrected JSON response.",
			previous.SPARQL,
			strings.Join(errorsList, "\n- "),
			instructions,
		)
	}
	systemPrompt := s.promptTemplate
	if s.ontologyPrompt != "" {
		systemPrompt = systemPrompt + "\n\n" + s.ontologyPrompt
	}

	return []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: user},
	}
}

type ToolResult struct {
	Tool    string `json:"tool"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Data    string `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (r ToolResult) Trace() string {
	if r.OK {
		return fmt.Sprintf("%s: %s", r.Tool, r.Summary)
	}
	return fmt.Sprintf("%s failed: %s", r.Tool, r.Error)
}

func (s *Server) toolDefinitions() []ToolDefinition {
	stringSchema := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	integerSchema := func(description string, defaultValue int) map[string]any {
		return map[string]any{"type": "integer", "description": description, "default": defaultValue, "minimum": 1, "maximum": 100}
	}
	objectSchema := func(required []string, properties map[string]any) map[string]any {
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             required,
			"properties":           properties,
		}
	}

	return []ToolDefinition{
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "search_rdf_examples",
				Description: "Search curated local RDF/Turtle example files for vocabulary, paths, labels, or IDs relevant to the user request, including record-specific files such as person.ttl, institution.ttl, source.ttl, and source-patterns.ttl.",
				Parameters: objectSchema([]string{"query"}, map[string]any{
					"query": stringSchema("ripgrep query string"),
					"limit": integerSchema("maximum number of matches", 8),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "search_rism_ontology",
				Description: "Search the RISM service ontology for classes, predicates, labels, comments, and rism:queryPattern annotations relevant to a query request.",
				Parameters: objectSchema([]string{"query"}, map[string]any{
					"query": stringSchema("ontology search string, such as ManuscriptSource, holding institution, incipit, external resource, or hasCountryCodes"),
					"limit": integerSchema("maximum number of matches", 10),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "read_rdf_example",
				Description: "Read a curated RDF example file by relative path after search identifies the relevant record-specific Turtle file.",
				Parameters: objectSchema([]string{"path"}, map[string]any{
					"path": stringSchema("relative path inside the RDF examples directory"),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "list_common_predicates",
				Description: "List common predicates in the Linked RISM triplestore with counts.",
				Parameters: objectSchema([]string{}, map[string]any{
					"limit": integerSchema("maximum number of predicates", 25),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "list_common_classes",
				Description: "List common rdf:type values in the Linked RISM triplestore with counts.",
				Parameters: objectSchema([]string{}, map[string]any{
					"limit": integerSchema("maximum number of classes", 25),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "sample_predicates_for_class",
				Description: "Sample predicates and example values used by subjects of a class URI.",
				Parameters: objectSchema([]string{"class_uri"}, map[string]any{
					"class_uri": stringSchema("class URI such as https://rism.online/api/v1#Source"),
					"limit":     integerSchema("maximum samples", 25),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "sample_triples_for_subject",
				Description: "Sample predicate/object pairs for a known subject URI.",
				Parameters: objectSchema([]string{"subject_uri"}, map[string]any{
					"subject_uri": stringSchema("subject URI"),
					"limit":       integerSchema("maximum samples", 25),
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "validate_sparql",
				Description: "Run deterministic read-only validation and LIMIT enforcement for a draft SPARQL query.",
				Parameters: objectSchema([]string{"query"}, map[string]any{
					"query": stringSchema("draft SPARQL query"),
				}),
			},
		},
	}
}

func (s *Server) executeToolCall(ctx context.Context, call ToolCall) ToolResult {
	name := call.Function.Name
	logDebug("executing tool call: id=%s name=%s args=%s", call.ID, name, truncateForDebug(call.Function.Arguments, 500))

	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return ToolResult{Tool: name, OK: false, Error: "invalid JSON arguments: " + err.Error()}
	}

	switch name {
	case "search_rdf_examples":
		query, err := requiredStringArg(args, "query")
		if err != nil {
			return ToolResult{Tool: name, OK: false, Error: err.Error()}
		}
		limit := intArg(args, "limit", 8, 1, 50)
		return s.searchRDFExamples(ctx, query, limit)
	case "search_rism_ontology":
		query, err := requiredStringArg(args, "query")
		if err != nil {
			return ToolResult{Tool: name, OK: false, Error: err.Error()}
		}
		limit := intArg(args, "limit", 10, 1, 50)
		return s.searchRISMOntology(ctx, query, limit)
	case "read_rdf_example":
		path, err := requiredStringArg(args, "path")
		if err != nil {
			return ToolResult{Tool: name, OK: false, Error: err.Error()}
		}
		return s.readRDFExample(path)
	case "list_common_predicates":
		limit := intArg(args, "limit", 25, 1, 100)
		return s.cachedDiscovery(ctx, name, fmt.Sprintf("predicates:%d", limit), commonPredicatesQuery(limit))
	case "list_common_classes":
		limit := intArg(args, "limit", 25, 1, 100)
		return s.cachedDiscovery(ctx, name, fmt.Sprintf("classes:%d", limit), commonClassesQuery(limit))
	case "sample_predicates_for_class":
		classURI, err := requiredURIArg(args, "class_uri")
		if err != nil {
			return ToolResult{Tool: name, OK: false, Error: err.Error()}
		}
		limit := intArg(args, "limit", 25, 1, 100)
		return s.cachedDiscovery(ctx, name, fmt.Sprintf("class-predicates:%s:%d", classURI, limit), samplePredicatesForClassQuery(classURI, limit))
	case "sample_triples_for_subject":
		subjectURI, err := requiredURIArg(args, "subject_uri")
		if err != nil {
			return ToolResult{Tool: name, OK: false, Error: err.Error()}
		}
		limit := intArg(args, "limit", 25, 1, 100)
		return s.cachedDiscovery(ctx, name, fmt.Sprintf("subject-triples:%s:%d", subjectURI, limit), sampleTriplesForSubjectQuery(subjectURI, limit))
	case "validate_sparql":
		query, err := requiredStringArg(args, "query")
		if err != nil {
			return ToolResult{Tool: name, OK: false, Error: err.Error()}
		}
		validation := validateSPARQL(query, s.config.DefaultLimit)
		data, _ := json.Marshal(validation)
		return ToolResult{Tool: name, OK: validation.Valid, Summary: validationSummary(validation), Data: string(data), Error: strings.Join(validation.Errors, "; ")}
	default:
		return ToolResult{Tool: name, OK: false, Error: "unknown tool"}
	}
}

func requiredStringArg(args map[string]json.RawMessage, name string) (string, error) {
	raw, ok := args[name]
	if !ok {
		return "", fmt.Errorf("missing %q argument", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%q must be a non-empty string", name)
	}
	return strings.TrimSpace(value), nil
}

func requiredURIArg(args map[string]json.RawMessage, name string) (string, error) {
	value, err := requiredStringArg(args, name)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%q must be an http(s) URI", name)
	}
	return value, nil
}

func intArg(args map[string]json.RawMessage, name string, fallback, minValue, maxValue int) int {
	raw, ok := args[name]
	if !ok {
		return fallback
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func validationSummary(validation ValidationResult) string {
	if validation.Valid {
		return "query passed deterministic validation"
	}
	return "query failed deterministic validation"
}

func isPlanningPlaceholder(generated GeneratedQuery) bool {
	text := strings.ToLower(strings.Join(append(append([]string{generated.Explanation}, generated.Assumptions...), generated.Warnings...), " "))
	planningPhrases := []string{
		"placeholder",
		"need to inspect",
		"needs to inspect",
		"i need to inspect",
		"i will first inspect",
		"before writing the sparql",
		"before writing the query",
		"while i inspect",
		"verify the rdf paths before writing",
		"not the final query",
	}
	for _, phrase := range planningPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func (s *Server) searchRDFExamples(ctx context.Context, query string, limit int) ToolResult {
	if _, err := os.Stat(s.config.RDFExamplesDir); err != nil {
		return ToolResult{Tool: "search_rdf_examples", OK: false, Error: fmt.Sprintf("RDF examples directory unavailable: %s", err)}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, s.config.RGPath, "--json", "--max-count", strconv.Itoa(limit), query, s.config.RDFExamplesDir)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return ToolResult{Tool: "search_rdf_examples", OK: false, Error: "ripgrep search timed out"}
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return ToolResult{Tool: "search_rdf_examples", OK: true, Summary: "no matches", Data: ""}
		}
		return ToolResult{Tool: "search_rdf_examples", OK: false, Error: fmt.Sprintf("ripgrep failed: %s; output: %s", err, truncateForDebug(string(output), 500))}
	}

	data := parseRGMatches(output, limit)
	return ToolResult{Tool: "search_rdf_examples", OK: true, Summary: fmt.Sprintf("returned up to %d match(es)", limit), Data: truncateForDebug(data, maxToolResultBytes)}
}

func (s *Server) searchRISMOntology(ctx context.Context, query string, limit int) ToolResult {
	if strings.TrimSpace(s.config.OntologyPath) == "" {
		return ToolResult{Tool: "search_rism_ontology", OK: false, Error: "ontology path is not configured"}
	}
	if _, err := os.Stat(s.config.OntologyPath); err != nil {
		return ToolResult{Tool: "search_rism_ontology", OK: false, Error: fmt.Sprintf("ontology unavailable: %s", err)}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, s.config.RGPath, "--json", "--ignore-case", "--context", "2", "--max-count", strconv.Itoa(limit), query, s.config.OntologyPath)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return ToolResult{Tool: "search_rism_ontology", OK: false, Error: "ontology search timed out"}
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return ToolResult{Tool: "search_rism_ontology", OK: true, Summary: "no matches", Data: ""}
		}
		return ToolResult{Tool: "search_rism_ontology", OK: false, Error: fmt.Sprintf("ripgrep failed: %s; output: %s", err, truncateForDebug(string(output), 500))}
	}

	data := parseRGMatches(output, limit)
	return ToolResult{Tool: "search_rism_ontology", OK: true, Summary: fmt.Sprintf("returned up to %d ontology match(es)", limit), Data: truncateForDebug(data, maxToolResultBytes)}
}

func parseRGMatches(output []byte, limit int) string {
	type rgPath struct {
		Text string `json:"text"`
	}
	type rgLines struct {
		Text string `json:"text"`
	}
	type rgData struct {
		Path       rgPath  `json:"path"`
		LineNumber int     `json:"line_number"`
		Lines      rgLines `json:"lines"`
	}
	type rgEvent struct {
		Type string `json:"type"`
		Data rgData `json:"data"`
	}

	var builder strings.Builder
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var event rgEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Type != "match" {
			continue
		}
		count++
		builder.WriteString(fmt.Sprintf("%s:%d: %s", event.Data.Path.Text, event.Data.LineNumber, strings.TrimSpace(event.Data.Lines.Text)))
		builder.WriteString("\n")
		if count >= limit {
			break
		}
	}
	return strings.TrimSpace(builder.String())
}

func (s *Server) readRDFExample(relativePath string) ToolResult {
	fullPath, err := safeJoin(s.config.RDFExamplesDir, relativePath)
	if err != nil {
		return ToolResult{Tool: "read_rdf_example", OK: false, Error: err.Error()}
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return ToolResult{Tool: "read_rdf_example", OK: false, Error: err.Error()}
	}
	return ToolResult{Tool: "read_rdf_example", OK: true, Summary: fmt.Sprintf("read %s", relativePath), Data: truncateForDebug(string(content), maxToolResultBytes)}
}

func safeJoin(root, relativePath string) (string, error) {
	if strings.TrimSpace(relativePath) == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(relativePath) {
		return "", errors.New("absolute paths are not allowed")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(filepath.Join(rootAbs, relativePath))
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(os.PathSeparator)) {
		return "", errors.New("path escapes RDF examples directory")
	}
	return fullAbs, nil
}

func (s *Server) cachedDiscovery(ctx context.Context, toolName, cacheKey, query string) ToolResult {
	now := time.Now()
	s.cacheMu.Lock()
	if entry, ok := s.discoveryCache[cacheKey]; ok && now.Before(entry.ExpiresAt) {
		s.cacheMu.Unlock()
		return ToolResult{Tool: toolName, OK: true, Summary: "returned cached discovery result", Data: entry.Value}
	}
	s.cacheMu.Unlock()

	data, err := s.runDiscoveryQuery(ctx, query)
	if err != nil {
		return ToolResult{Tool: toolName, OK: false, Error: err.Error()}
	}
	data = truncateForDebug(data, maxToolResultBytes)

	s.cacheMu.Lock()
	s.discoveryCache[cacheKey] = cacheEntry{Value: data, ExpiresAt: now.Add(s.config.DiscoveryCacheTTL)}
	s.cacheMu.Unlock()

	return ToolResult{Tool: toolName, OK: true, Summary: "queried Linked RISM discovery data", Data: data}
}

func (s *Server) runDiscoveryQuery(ctx context.Context, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	body := strings.NewReader("query=" + url.QueryEscape(query))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.SPARQLEndpoint, body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/sparql-results+json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if err != nil {
		return "", err
	}
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("discovery query returned HTTP %d: %s", response.StatusCode, truncateForDebug(string(responseBody), 500))
	}
	return string(responseBody), nil
}

func commonPredicatesQuery(limit int) string {
	return fmt.Sprintf("SELECT ?p (COUNT(*) AS ?count) WHERE { ?s ?p ?o } GROUP BY ?p ORDER BY DESC(?count) LIMIT %d", limit)
}

func commonClassesQuery(limit int) string {
	return fmt.Sprintf("SELECT ?class (COUNT(*) AS ?count) WHERE { ?s a ?class } GROUP BY ?class ORDER BY DESC(?count) LIMIT %d", limit)
}

func samplePredicatesForClassQuery(classURI string, limit int) string {
	return fmt.Sprintf("SELECT ?p ?sample WHERE { ?s a <%s> ; ?p ?sample . } LIMIT %d", classURI, limit)
}

func sampleTriplesForSubjectQuery(subjectURI string, limit int) string {
	return fmt.Sprintf("SELECT ?p ?o WHERE { <%s> ?p ?o . } LIMIT %d", subjectURI, limit)
}

func validateSPARQL(query string, defaultLimit int) ValidationResult {
	normalized := strings.TrimSpace(stripCodeFence(query))
	result := ValidationResult{Query: normalized, Valid: true}

	if normalized == "" {
		return invalidResult(normalized, "SPARQL query is empty")
	}
	if len(normalized) > maxQueryLength {
		return invalidResult(normalized, "SPARQL query is too long")
	}
	if destructiveQueryPattern.MatchString(normalized) {
		return invalidResult(normalized, "SPARQL query contains an update or destructive keyword")
	}

	match := queryTypePattern.FindStringSubmatch(normalized)
	if len(match) < 2 {
		return invalidResult(normalized, "SPARQL query must be read-only and start with SELECT, ASK, CONSTRUCT, or DESCRIBE after PREFIX declarations")
	}

	queryType := strings.ToUpper(match[1])
	if queryType != "ASK" && !limitPattern.MatchString(normalized) {
		normalized = strings.TrimRight(normalized, " \t\r\n;")
		normalized = fmt.Sprintf("%s\nLIMIT %d", normalized, defaultLimit)
	}

	result.Query = normalized
	return result
}

func invalidResult(query, message string) ValidationResult {
	return ValidationResult{Query: query, Valid: false, Errors: []string{message}}
}

func retryState(hasRetryRemaining bool) string {
	if hasRetryRemaining {
		return "retry"
	}
	return "error"
}

func retryMessage(message string, hasRetryRemaining bool) string {
	if hasRetryRemaining {
		return message + "; retrying"
	}
	return message
}

func stripCodeFence(value string) string {
	match := codeFencePattern.FindStringSubmatch(value)
	if len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return value
}

func (s *Server) validateAgainstEndpoint(ctx context.Context, query string) ValidationResult {
	body := strings.NewReader("query=" + url.QueryEscape(query))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.SPARQLEndpoint, body)
	if err != nil {
		return invalidResult(query, err.Error())
	}
	request.Header.Set("Accept", "application/sparql-results+json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := s.client.Do(request)
	if err != nil {
		return invalidResult(query, fmt.Sprintf("endpoint validation failed: %s", err))
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return invalidResult(query, fmt.Sprintf("endpoint validation returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody))))
	}
	return ValidationResult{Query: query, Valid: true}
}

func (p *InfomaniakProvider) Complete(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (LLMCompletion, error) {
	payload := infomaniakRequest{
		Model:               p.config.InfomaniakModel,
		Messages:            messages,
		Temperature:         0,
		MaxCompletionTokens: 4000,
		Stream:              false,
		PromptCacheKey:      p.config.PromptCacheKey,
		ReasoningEffort:     p.config.InfomaniakReasoningEffort,
		ParallelToolCalls:   false,
		Tools:               tools,
		ToolChoice:          "auto",
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":        "sparql_generation",
				"description": "A generated Linked RISM SPARQL query with explanation and notes.",
				"strict":      true,
				"schema": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"sparql", "explanation", "assumptions", "warnings", "tool_trace"},
					"properties": map[string]any{
						"sparql": map[string]any{
							"type":        "string",
							"description": "The generated read-only SPARQL query.",
						},
						"explanation": map[string]any{
							"type":        "string",
							"description": "Brief explanation of what the query does.",
						},
						"assumptions": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
						"warnings": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
						"tool_trace": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	requestBody, err := json.Marshal(payload)
	if err != nil {
		return LLMCompletion{}, err
	}

	url := fmt.Sprintf("https://api.infomaniak.com/2/ai/%s/openai/v1/chat/completions", p.config.InfomaniakProductID)
	logDebug("Infomaniak request: model=%s reasoning_effort=%s messages=%d tools=%d payload_bytes=%d", p.config.InfomaniakModel, p.config.InfomaniakReasoningEffort, len(messages), len(tools), len(requestBody))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return LLMCompletion{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.config.InfomaniakAPIToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := p.client.Do(request)
	if err != nil {
		return LLMCompletion{}, err
	}
	defer response.Body.Close()
	logDebug("Infomaniak response status: http_status=%d", response.StatusCode)

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return LLMCompletion{}, err
	}
	logDebug("Infomaniak response body: bytes=%d snippet=%q", len(responseBody), truncateForDebug(string(responseBody), 1000))
	if response.StatusCode >= 400 {
		return LLMCompletion{}, fmt.Errorf("Infomaniak returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var completion infomaniakResponse
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return LLMCompletion{}, errors.New("Infomaniak response was not valid JSON")
	}
	if len(completion.Choices) == 0 {
		return LLMCompletion{}, errors.New("Infomaniak response did not include choices")
	}

	message := completion.Choices[0].Message
	if len(message.ToolCalls) > 0 {
		logDebug("Infomaniak returned %d tool call(s)", len(message.ToolCalls))
		return LLMCompletion{ToolCalls: message.ToolCalls}, nil
	}

	reasoning := strings.TrimSpace(firstNonEmpty(message.Reasoning, message.ReasoningContent))
	if message.Content == nil || strings.TrimSpace(*message.Content) == "" {
		logDebug("Infomaniak response had empty content: reasoning_bytes=%d reasoning_snippet=%q", len(reasoning), truncateForDebug(reasoning, 1000))
		return LLMCompletion{}, errors.New("Infomaniak response did not include message content; reasoning output was returned instead. Try INFOMANIAK_REASONING_EFFORT=none or a non-reasoning model")
	}

	logDebug("Infomaniak message content: bytes=%d snippet=%q", len(*message.Content), truncateForDebug(*message.Content, 1000))
	generated, err := parseGeneratedQuery(*message.Content, p.debugLLM)
	if err != nil {
		return LLMCompletion{}, err
	}
	return LLMCompletion{Generated: &generated}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseGeneratedQuery(content string, debug bool) (GeneratedQuery, error) {
	content = extractJSONObject(stripCodeFence(content))
	var raw rawGeneratedQuery
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		logDebug("LLM parse failed: error=%s content_size=%d snippet=%q", err, len(content), truncateForDebug(content, 1000))
		if debug {
			return GeneratedQuery{}, LLMParseError{
				Message:     fmt.Sprintf("LLM output was not valid generation JSON: %s", err),
				RawSnippet:  truncateForDebug(content, 2000),
				ContentSize: len(content),
				Kind:        "invalid_json",
			}
		}
		return GeneratedQuery{}, errors.New("LLM output was not valid generation JSON")
	}

	assumptions, err := parseStringList(raw.Assumptions)
	if err != nil {
		return parseGeneratedQueryFieldError("assumptions", err, content, debug)
	}
	warnings, err := parseStringList(raw.Warnings)
	if err != nil {
		return parseGeneratedQueryFieldError("warnings", err, content, debug)
	}
	toolTrace, err := parseStringList(raw.ToolTrace)
	if err != nil {
		return parseGeneratedQueryFieldError("tool_trace", err, content, debug)
	}

	generated := GeneratedQuery{
		SPARQL:      raw.SPARQL,
		Explanation: raw.Explanation,
		Assumptions: assumptions,
		Warnings:    warnings,
		ToolTrace:   toolTrace,
	}
	generated.SPARQL = strings.TrimSpace(generated.SPARQL)
	generated.Explanation = strings.TrimSpace(generated.Explanation)
	if generated.SPARQL == "" {
		logDebug("LLM parse failed: missing sparql field content_size=%d snippet=%q", len(content), truncateForDebug(content, 1000))
		if debug {
			return GeneratedQuery{}, LLMParseError{
				Message:     "LLM output JSON did not include a non-empty sparql field",
				RawSnippet:  truncateForDebug(content, 2000),
				ContentSize: len(content),
				Kind:        "empty_sparql",
			}
		}
		return GeneratedQuery{}, LLMParseError{Message: "LLM output JSON did not include a non-empty sparql field", Kind: "empty_sparql"}
	}
	return generated, nil
}

func parseGeneratedQueryFieldError(field string, err error, content string, debug bool) (GeneratedQuery, error) {
	message := fmt.Sprintf("LLM output field %q was invalid: %s", field, err)
	logDebug("LLM parse failed: %s content_size=%d snippet=%q", message, len(content), truncateForDebug(content, 1000))
	if debug {
		return GeneratedQuery{}, LLMParseError{
			Message:     message,
			RawSnippet:  truncateForDebug(content, 2000),
			ContentSize: len(content),
		}
	}
	return GeneratedQuery{}, errors.New(message)
}

func parseStringList(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}

	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		return trimStringList(values), nil
	}

	var singleValue string
	if err := json.Unmarshal(raw, &singleValue); err == nil {
		singleValue = strings.TrimSpace(singleValue)
		if singleValue == "" {
			return nil, nil
		}
		return []string{singleValue}, nil
	}

	return nil, errors.New("expected a string or array of strings")
}

func trimStringList(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func truncateForDebug(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "... [truncated]"
}

func extractJSONObject(content string) string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "{") && strings.HasSuffix(content, "}") {
		return content
	}

	start := strings.Index(content, "{")
	if start < 0 {
		return content
	}

	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(content); index++ {
		char := content[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}

		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return strings.TrimSpace(content[start : index+1])
			}
		}
	}

	return content
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("failed to write JSON response: %s", err)
	}
}
