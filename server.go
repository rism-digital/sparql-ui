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
	"regexp"
	"strings"
	"time"
)

const (
	defaultAddr           = "127.0.0.1:8787"
	defaultModel          = "moonshotai/Kimi-K2.6"
	defaultSPARQLEndpoint = "https://linked.rism.io/api"
	defaultPromptPath     = "prompts/rism-nl2sparql.md"
	defaultMaxRetries     = 2
	defaultQueryLimit     = 100
	maxInstructionLength  = 4000
	maxQueryLength        = 20000
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
	EnableEndpointValidation  bool
	DebugLLM                  bool
	LogLevel                  LogLevel
	MaxRepairRetries          int
	DefaultLimit              int
}

type Server struct {
	config         Config
	client         *http.Client
	provider       LLMProvider
	promptTemplate string
}

type LLMProvider interface {
	Complete(ctx context.Context, messages []ChatMessage) (GeneratedQuery, error)
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type GenerateRequest struct {
	Instructions string `json:"instructions"`
}

type GeneratedQuery struct {
	SPARQL      string   `json:"sparql"`
	Explanation string   `json:"explanation"`
	Assumptions []string `json:"assumptions"`
	Warnings    []string `json:"warnings"`
}

type rawGeneratedQuery struct {
	SPARQL      string          `json:"sparql"`
	Explanation string          `json:"explanation"`
	Assumptions json.RawMessage `json:"assumptions"`
	Warnings    json.RawMessage `json:"warnings"`
}

type GenerateResponse struct {
	SPARQL      string   `json:"sparql"`
	Explanation string   `json:"explanation"`
	Assumptions []string `json:"assumptions"`
	Warnings    []string `json:"warnings"`
	Valid       bool     `json:"valid"`
	Attempts    int      `json:"attempts"`
	Errors      []string `json:"errors,omitempty"`
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
}

func (e LLMParseError) Error() string {
	if e.RawSnippet == "" {
		return e.Message
	}
	return fmt.Sprintf("%s; raw content size=%d; raw content snippet=%q", e.Message, e.ContentSize, e.RawSnippet)
}

type infomaniakRequest struct {
	Model               string         `json:"model"`
	Messages            []ChatMessage  `json:"messages"`
	Temperature         float64        `json:"temperature"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
	Stream              bool           `json:"stream"`
	PromptCacheKey      string         `json:"prompt_cache_key,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	ResponseFormat      map[string]any `json:"response_format,omitempty"`
	ParallelToolCalls   bool           `json:"parallel_tool_calls"`
}

type infomaniakResponse struct {
	Choices []struct {
		Message struct {
			Content          *string `json:"content"`
			Reasoning        string  `json:"reasoning"`
			ReasoningContent string  `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Printf("Skipping .env load: %s", err)
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
	server := &Server{
		config:         config,
		client:         client,
		promptTemplate: promptTemplate,
	}
	server.provider = &InfomaniakProvider{config: config, client: client, debugLLM: config.DebugLLM}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealth)
	mux.HandleFunc("/api/generate-sparql", server.handleGenerateSPARQL)

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
		PromptTemplatePath:        envOrDefault("PROMPT_TEMPLATE_PATH", defaultPromptPath),
		PromptCacheKey:            envOrDefault("PROMPT_CACHE_KEY", "linked-rism-nl2sparql-v1"),
		EnableEndpointValidation:  strings.EqualFold(os.Getenv("ENABLE_ENDPOINT_VALIDATION"), "true"),
		DebugLLM:                  strings.EqualFold(os.Getenv("DEBUG_LLM"), "true"),
		LogLevel:                  parseLogLevel(envOrDefault("LOG_LEVEL", "info")),
		MaxRepairRetries:          intEnvOrDefault("MAX_REPAIR_RETRIES", defaultMaxRetries),
		DefaultLimit:              intEnvOrDefault("DEFAULT_QUERY_LIMIT", defaultQueryLimit),
	}
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
	if s.config.InfomaniakAPIToken == "" || s.config.InfomaniakProductID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Infomaniak API configuration is missing"})
		return
	}

	logDebug("generate request received: instruction_length=%d", len(instructions))
	response, err := s.generateValidatedQuery(r.Context(), instructions)
	if err != nil {
		logError("generate request failed: %s", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	logDebug("generate request completed: valid=%t attempts=%d sparql_length=%d errors=%d", response.Valid, response.Attempts, len(response.SPARQL), len(response.Errors))
	writeJSON(w, http.StatusOK, response)
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

func (s *Server) generateValidatedQuery(ctx context.Context, instructions string) (GenerateResponse, error) {
	var validation ValidationResult
	var generated GeneratedQuery
	var err error
	attempts := 0

	for attempt := 0; attempt <= s.config.MaxRepairRetries; attempt++ {
		attempts = attempt + 1
		logDebug("generation attempt %d started: repair=%t", attempts, attempt > 0)
		messages := s.buildMessages(instructions, generated, validation.Errors, attempt > 0)
		generated, err = s.provider.Complete(ctx, messages)
		if err != nil {
			logDebug("generation attempt %d provider error: %s", attempts, err)
			return GenerateResponse{}, err
		}
		logDebug("generation attempt %d returned query: sparql_length=%d explanation_length=%d", attempts, len(generated.SPARQL), len(generated.Explanation))

		validation = validateSPARQL(generated.SPARQL, s.config.DefaultLimit)
		if validation.Valid && s.config.EnableEndpointValidation {
			logDebug("generation attempt %d running endpoint validation", attempts)
			validation = s.validateAgainstEndpoint(ctx, validation.Query)
		}
		if validation.Valid {
			logDebug("generation attempt %d validation succeeded", attempts)
			generated.SPARQL = validation.Query
			return GenerateResponse{
				SPARQL:      generated.SPARQL,
				Explanation: generated.Explanation,
				Assumptions: generated.Assumptions,
				Warnings:    generated.Warnings,
				Valid:       true,
				Attempts:    attempts,
			}, nil
		}
		logDebug("generation attempt %d validation failed: %s", attempts, strings.Join(validation.Errors, "; "))
	}

	return GenerateResponse{
		SPARQL:      generated.SPARQL,
		Explanation: generated.Explanation,
		Assumptions: generated.Assumptions,
		Warnings:    generated.Warnings,
		Valid:       false,
		Attempts:    attempts,
		Errors:      validation.Errors,
	}, nil
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

	return []ChatMessage{
		{Role: "system", Content: s.promptTemplate},
		{Role: "user", Content: user},
	}
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

func (p *InfomaniakProvider) Complete(ctx context.Context, messages []ChatMessage) (GeneratedQuery, error) {
	payload := infomaniakRequest{
		Model:               p.config.InfomaniakModel,
		Messages:            messages,
		Temperature:         0,
		MaxCompletionTokens: 4000,
		Stream:              false,
		PromptCacheKey:      p.config.PromptCacheKey,
		ReasoningEffort:     p.config.InfomaniakReasoningEffort,
		ParallelToolCalls:   false,
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":        "sparql_generation",
				"description": "A generated Linked RISM SPARQL query with explanation and notes.",
				"strict":      true,
				"schema": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"sparql", "explanation", "assumptions", "warnings"},
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
					},
				},
			},
		},
	}

	requestBody, err := json.Marshal(payload)
	if err != nil {
		return GeneratedQuery{}, err
	}

	url := fmt.Sprintf("https://api.infomaniak.com/2/ai/%s/openai/v1/chat/completions", p.config.InfomaniakProductID)
	logDebug("Infomaniak request: model=%s reasoning_effort=%s messages=%d payload_bytes=%d", p.config.InfomaniakModel, p.config.InfomaniakReasoningEffort, len(messages), len(requestBody))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return GeneratedQuery{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.config.InfomaniakAPIToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := p.client.Do(request)
	if err != nil {
		return GeneratedQuery{}, err
	}
	defer response.Body.Close()
	logDebug("Infomaniak response status: http_status=%d", response.StatusCode)

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return GeneratedQuery{}, err
	}
	logDebug("Infomaniak response body: bytes=%d snippet=%q", len(responseBody), truncateForDebug(string(responseBody), 1000))
	if response.StatusCode >= 400 {
		return GeneratedQuery{}, fmt.Errorf("Infomaniak returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var completion infomaniakResponse
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return GeneratedQuery{}, errors.New("Infomaniak response was not valid JSON")
	}
	if len(completion.Choices) == 0 {
		return GeneratedQuery{}, errors.New("Infomaniak response did not include choices")
	}

	message := completion.Choices[0].Message
	reasoning := strings.TrimSpace(firstNonEmpty(message.Reasoning, message.ReasoningContent))
	if message.Content == nil || strings.TrimSpace(*message.Content) == "" {
		logDebug("Infomaniak response had empty content: reasoning_bytes=%d reasoning_snippet=%q", len(reasoning), truncateForDebug(reasoning, 1000))
		return GeneratedQuery{}, errors.New("Infomaniak response did not include message content; reasoning output was returned instead. Try INFOMANIAK_REASONING_EFFORT=none or a non-reasoning model")
	}

	logDebug("Infomaniak message content: bytes=%d snippet=%q", len(*message.Content), truncateForDebug(*message.Content, 1000))
	return parseGeneratedQuery(*message.Content, p.debugLLM)
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

	generated := GeneratedQuery{
		SPARQL:      raw.SPARQL,
		Explanation: raw.Explanation,
		Assumptions: assumptions,
		Warnings:    warnings,
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
			}
		}
		return GeneratedQuery{}, errors.New("LLM output JSON did not include a non-empty sparql field")
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
