# Linked RISM SPARQL UI

Simple static SPARQL query page using `@sib-swiss/sparql-editor` and a custom examples sidebar.

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

- This implementation intentionally does **not** use the `examples-repository` feature from `sparql-editor`.
- Sidebar actions:
  - `Load`: inserts query into the editor as a new tab.
  - `Copy`: copies query text to clipboard.
