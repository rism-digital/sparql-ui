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
