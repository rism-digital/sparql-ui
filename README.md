# Linked RISM SPARQL UI

Simple static SPARQL query page using plain `YASGUI` and a custom examples sidebar.

## Endpoint

The editor is configured for:

- `https://linked.rism.io/api`

## Examples workflow

1. Add query files to `examples/` with extension `.rq`.
2. First non-empty line must be:

```text
# Title: Your Example Title
```

3. Remaining file content is used as query text.
4. Compile examples:

```bash
./scripts/compile-examples.sh
```

This generates `examples.json`, which the page loads at runtime.

## Run locally

After generating `examples.json`, serve the folder with a local web server:

```bash
python -m http.server
```

Then open `http://localhost:8000`.

## Natural language query prototype

The natural language query page is available at:

- `query.html`

It expects a separate local Go JSON API. The Go server does not serve static files; it only talks to the LLM provider and returns generated SPARQL as JSON.

Configure the backend:

```bash
export INFOMANIAK_API_TOKEN="..."
export INFOMANIAK_PRODUCT_ID="..."
export INFOMANIAK_MODEL="moonshotai/Kimi-K2.6"
```

Alternatively, create a local `.env` file in the project root:

```text
INFOMANIAK_API_TOKEN=...
INFOMANIAK_PRODUCT_ID=...
INFOMANIAK_MODEL=moonshotai/Kimi-K2.6
INFOMANIAK_REASONING_EFFORT=none
```

Values already present in the process environment take precedence over `.env`.

Run the backend:

```bash
cd server
go run .
```

You can also launch it from the repo root with `go run ./server`.

Defaults:

- backend URL: `http://127.0.0.1:8787`
- SPARQL endpoint: `https://linked.rism.io/api`
- prompt cache key: `linked-rism-nl2sparql-v1`

Optional backend settings:

- `NL2SPARQL_ADDR`: bind address, defaults to `127.0.0.1:8787`
- `INFOMANIAK_REASONING_EFFORT`: defaults to `none`; keeps reasoning models from spending output on hidden reasoning instead of JSON content
- `SPARQL_ENDPOINT`: endpoint included in prompts and optional validation
- `PROMPT_TEMPLATE_PATH`: prompt template loaded on startup, defaults to `prompts/rism-nl2sparql.md` and is resolved relative to the current working directory or its parent
- `PROMPT_CACHE_KEY`: stable provider prompt-cache key
- `RDF_EXAMPLES_DIR`: local RDF/example search directory, defaults to `rdf-examples` and is resolved relative to the current working directory or its parent
- `ONTOLOGY_PATH`: RISM service ontology used for prompt context and ontology search, defaults to `rdf-examples/rism-service-ontology.ttl` and is resolved relative to the current working directory or its parent
- `RG_PATH`: ripgrep executable path, defaults to `rg`
- `MAX_TOOL_ITERATIONS`: maximum LLM tool-call rounds per generation attempt, defaults to `4`
- `DISCOVERY_CACHE_TTL_SECONDS`: cache TTL for Linked RISM discovery tool results, defaults to `3600`
- `MAX_REPAIR_RETRIES`: generation repair attempts, defaults to `2`
- `DEFAULT_QUERY_LIMIT`: limit added to generated read queries, defaults to `100`
- `ENABLE_ENDPOINT_VALIDATION=true`: additionally submit generated SPARQL to the endpoint for validation
- `LOG_LEVEL=debug`: print detailed backend progress, provider status, response snippets, and parser diagnostics
- `DEBUG_LLM=true`: include raw LLM content snippets in parse errors for local debugging

The NL-to-SPARQL backend exposes a coding harness to the LLM with tools for:

- searching `rdf-examples/` with ripgrep across record-specific Turtle files such as `person.ttl`, `place.ttl`, `institution.ttl`, `source.ttl`, `work.ttl`, `publication.ttl`, and `source-patterns.ttl`
- searching the configured RISM service ontology for labels, comments, and `rism:queryPattern` annotations
- reading a selected RDF example file
- listing common Linked RISM predicates/classes
- sampling predicates for a class or triples for a subject
- validating draft SPARQL

The intended split is:

- `rism-service-ontology.ttl` for classes, predicates, comments, and query-pattern hints
- record-specific `.ttl` files for concrete RDF structure by entity type
- `source-patterns.ttl` for cross-cutting source, relationship, and holding patterns

The local RDF search tool requires `rg` (ripgrep) to be installed, or `RG_PATH` must point to a compatible executable.

## Notes

- The page uses plain YASGUI (not `@sib-swiss/sparql-editor`).
- SPARQL queries are sent using HTTP `POST`.
- `SELECT` queries without explicit `LIMIT`/`OFFSET` are server-paged with `LIMIT 100 OFFSET n`.
- `CONSTRUCT` queries without explicit `LIMIT` get `LIMIT 100` automatically.
- If a query already includes `LIMIT` or `OFFSET`, it is sent unchanged.
- Sidebar actions:
  - `Load`: inserts query into the editor (new tab when supported by current YASGUI API).
  - `Copy`: copies query text to clipboard.

## Generating a void file:

```
java -jar void-generator-0.19-uber.jar -r "https://linked.rism.io/api" -p "https://linked.rism.io/api" --void-file void-rism.ttl --iri-of-void 'https://linked.rism.io/.well-known/void#' -g "http://linked.rism.io/" --optimize-for=Qlever
```
