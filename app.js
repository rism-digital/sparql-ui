const EXAMPLES_JSON_PATH = "./examples.json";

const toggleBtn = document.getElementById("toggleSidebarBtn");
const statusEl = document.getElementById("examplesStatus");
const listEl = document.getElementById("examplesList");
const editorEl = document.getElementById("sparqlEditor");

toggleBtn.addEventListener("click", () => {
  const isOpen = document.body.classList.contains("sidebar-open");
  document.body.classList.toggle("sidebar-open", !isOpen);
  document.body.classList.toggle("sidebar-collapsed", isOpen);
  toggleBtn.textContent = isOpen ? "Show examples" : "Hide examples";
  toggleBtn.setAttribute("aria-expanded", String(!isOpen));
});

function setStatus(message) {
  statusEl.textContent = message;
}

function truncatePreview(text) {
  const oneLine = text.replace(/\s+/g, " ").trim();
  return oneLine.length > 110 ? `${oneLine.slice(0, 110)}...` : oneLine;
}

function fallbackCopy(text) {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  let copied = false;
  try {
    copied = document.execCommand("copy");
  } finally {
    document.body.removeChild(textarea);
  }
  return copied;
}

async function copyQuery(example) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(example.query);
      setStatus(`Copied: ${example.title}`);
      return;
    }
    const copied = fallbackCopy(example.query);
    setStatus(copied ? `Copied: ${example.title}` : `Copy failed: ${example.title}`);
  } catch {
    setStatus(`Copy failed: ${example.title}`);
  }
}

function loadQuery(example) {
  if (typeof editorEl.addTab === "function") {
    editorEl.addTab(example.query, example.title);
    setStatus(`Loaded: ${example.title}`);
    return;
  }

  const yasqe = editorEl?.yasgui?.getTab?.()?.getYasqe?.();
  if (yasqe && typeof yasqe.setValue === "function") {
    yasqe.setValue(example.query);
    setStatus(`Loaded: ${example.title}`);
    return;
  }

  setStatus(`Could not load "${example.title}" into the editor.`);
}

function renderExamples(examples) {
  listEl.innerHTML = "";
  examples.forEach(example => {
    const card = document.createElement("article");
    card.className = "example-item";

    const title = document.createElement("h3");
    title.className = "example-title";
    title.textContent = example.title || example.id || "Untitled example";
    card.appendChild(title);

    const preview = document.createElement("p");
    preview.className = "example-preview";
    preview.textContent = truncatePreview(example.query || "");
    card.appendChild(preview);

    const actions = document.createElement("div");
    actions.className = "example-actions";

    const loadBtn = document.createElement("button");
    loadBtn.className = "btn btn-primary";
    loadBtn.type = "button";
    loadBtn.textContent = "Load";
    loadBtn.addEventListener("click", () => loadQuery(example));
    actions.appendChild(loadBtn);

    const copyBtn = document.createElement("button");
    copyBtn.className = "btn";
    copyBtn.type = "button";
    copyBtn.textContent = "Copy";
    copyBtn.addEventListener("click", () => {
      void copyQuery(example);
    });
    actions.appendChild(copyBtn);

    card.appendChild(actions);
    listEl.appendChild(card);
  });
}

async function loadExamples() {
  try {
    const response = await fetch(EXAMPLES_JSON_PATH, {cache: "no-store"});
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const examples = await response.json();
    if (!Array.isArray(examples)) {
      throw new Error("Invalid examples.json format");
    }
    if (examples.length === 0) {
      setStatus("No examples found. Add files in examples/ and run scripts/compile-examples.sh.");
      return;
    }
    renderExamples(examples);
    setStatus(`${examples.length} example(s) loaded.`);
  } catch (error) {
    listEl.innerHTML = "";
    setStatus(`Failed to load examples (${error.message}). Build examples.json first.`);
  }
}

async function init() {
  await customElements.whenDefined("sparql-editor");
  await loadExamples();
}

void init();
