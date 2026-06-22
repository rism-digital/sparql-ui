const BACKEND_URL = "http://127.0.0.1:8787";
const SPARQL_ENDPOINT = "https://linked.rism.io/api";
const DEFAULT_LIMIT = 100;

const instructionsInput = document.getElementById("instructionsInput");
const generateBtn = document.getElementById("generateBtn");
const clearBtn = document.getElementById("clearBtn");
const sparqlOutput = document.getElementById("sparqlOutput");
const copyBtn = document.getElementById("copyBtn");
const runBtn = document.getElementById("runBtn");
const apiStatus = document.getElementById("apiStatus");
const generationStatus = document.getElementById("generationStatus");
const explanationText = document.getElementById("explanationText");
const assumptionsList = document.getElementById("assumptionsList");
const warningsList = document.getElementById("warningsList");
const toolTraceList = document.getElementById("toolTraceList");
const resultsStatus = document.getElementById("resultsStatus");
const resultsTableWrap = document.getElementById("resultsTableWrap");
const streamStatus = document.getElementById("streamStatus");
const streamList = document.getElementById("streamList");
const progressEls = new Map(
  Array.from(document.querySelectorAll("#progressList [data-step]")).map(element => [element.dataset.step, element])
);

const destructiveQueryPattern = /\b(INSERT|DELETE|LOAD|CLEAR|CREATE|DROP|MOVE|COPY|ADD|WITH)\b/i;
const queryTypePattern = /^\s*(?:PREFIX\s+[A-Za-z][\w-]*:\s*<[^>]+>\s*)*(SELECT|ASK|CONSTRUCT|DESCRIBE)\b/is;
const limitPattern = /\bLIMIT\s+\d+\b/i;
let currentProgressSocket = null;
let currentRequestID = "";

generateBtn.addEventListener("click", () => {
  void generateSparql();
});

clearBtn.addEventListener("click", () => {
  closeProgressSocket();
  instructionsInput.value = "";
  sparqlOutput.value = "";
  explanationText.textContent = "No query generated yet.";
  renderList(assumptionsList, []);
  renderList(warningsList, []);
  renderList(toolTraceList, []);
  generationStatus.textContent = "Waiting for instructions.";
  resultsStatus.textContent = "No query run yet.";
  resultsTableWrap.innerHTML = "";
  resetProgress();
  resetStream();
  copyBtn.disabled = true;
  runBtn.disabled = true;
});

copyBtn.addEventListener("click", () => {
  void copySparql();
});

runBtn.addEventListener("click", () => {
  void runSparql();
});

sparqlOutput.addEventListener("input", () => {
  runBtn.disabled = sparqlOutput.value.trim() === "";
  copyBtn.disabled = sparqlOutput.value.trim() === "";
});

async function checkBackend() {
  updateProgress("backend", "active", "Checking");
  try {
    const response = await fetch(`${BACKEND_URL}/healthz`, {cache: "no-store"});
    apiStatus.textContent = response.ok ? "Backend: ready" : `Backend: HTTP ${response.status}`;
    updateProgress("backend", response.ok ? "done" : "error", response.ok ? "Ready" : `HTTP ${response.status}`);
  } catch {
    apiStatus.textContent = "Backend: unavailable";
    updateProgress("backend", "error", "Unavailable");
  }
}

async function generateSparql() {
  const instructions = instructionsInput.value.trim();
  if (!instructions) {
    generationStatus.textContent = "Enter query instructions first.";
    return;
  }

  setGenerating(true);
  closeProgressSocket();
  resetProgress({keepBackend: true});
  resetStream();
  updateProgress("request", "active", "Sending instructions");
  generationStatus.textContent = "Generating and validating...";
  resultsTableWrap.innerHTML = "";
  resultsStatus.textContent = "No query run yet.";

  try {
    const requestID = createRequestID();
    currentRequestID = requestID;
    const stream = openProgressSocket(requestID);
    await stream.ready;
    updateProgress("llm", "active", "Waiting for provider");
    const response = await fetch(`${BACKEND_URL}/api/generate-sparql`, {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify({request_id: requestID, instructions}),
    });
    updateProgress("request", "done", "Accepted");
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error || `HTTP ${response.status}`);
    }

    updateProgress("llm", "done", "Draft received");
    sparqlOutput.value = payload.sparql || "";
    explanationText.textContent = payload.explanation || "No explanation provided.";
    renderList(assumptionsList, payload.assumptions || []);
    renderList(warningsList, payload.warnings || []);
    renderList(toolTraceList, payload.tool_trace || []);
    if (payload.valid) {
      generationStatus.textContent = `Validated after ${payload.attempts || 1} attempt(s).`;
      updateProgress("validation", "done", `Validated in ${payload.attempts || 1} attempt(s)`);
    } else {
      generationStatus.textContent = `Validation failed: ${(payload.errors || []).join("; ")}`;
      updateProgress("validation", "error", "Rejected");
    }
    copyBtn.disabled = sparqlOutput.value.trim() === "";
    runBtn.disabled = !payload.valid || sparqlOutput.value.trim() === "";
  } catch (error) {
    generationStatus.textContent = `Generation failed: ${error.message}`;
    appendStreamMessage({state: "error", message: `Generation failed: ${error.message}`});
    updateProgress("request", "error", "Failed");
    updateProgress("llm", "error", "Failed");
    runBtn.disabled = true;
  } finally {
    setGenerating(false);
  }
}

function setGenerating(isGenerating) {
  generateBtn.disabled = isGenerating;
  generateBtn.textContent = isGenerating ? "Generating..." : "Generate SPARQL";
}

function createRequestID() {
  if (window.crypto?.randomUUID) {
    return window.crypto.randomUUID();
  }
  return `request-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function openProgressSocket(requestID) {
  if (!("WebSocket" in window)) {
    streamStatus.textContent = "Streaming unavailable";
    appendStreamMessage({state: "error", message: "This browser does not support websockets."});
    return {ready: Promise.resolve()};
  }

  const url = new URL(BACKEND_URL);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = "/api/generate-sparql/events";
  url.search = new URLSearchParams({request_id: requestID}).toString();

  streamStatus.textContent = "Connecting";
  appendStreamMessage({state: "active", message: "Opening progress stream."});
  const socket = new WebSocket(url);
  currentProgressSocket = socket;

  const ready = new Promise(resolve => {
    let settled = false;
    const settle = () => {
      if (settled) return;
      settled = true;
      resolve();
    };
    const timeout = window.setTimeout(() => {
      appendStreamMessage({state: "error", message: "Progress stream connection is slow; continuing with JSON request."});
      settle();
    }, 750);

    socket.addEventListener("open", () => {
      window.clearTimeout(timeout);
      if (currentProgressSocket !== socket) return;
      streamStatus.textContent = "Connected";
      appendStreamMessage({state: "done", message: "Progress stream connected."});
      settle();
    });

    socket.addEventListener("error", () => {
      window.clearTimeout(timeout);
      if (currentProgressSocket !== socket) return;
      streamStatus.textContent = "Streaming unavailable";
      appendStreamMessage({state: "error", message: "Progress stream unavailable; continuing with final JSON response."});
      settle();
    });
  });

  socket.addEventListener("message", event => {
    if (currentProgressSocket !== socket) return;
    try {
      const payload = JSON.parse(event.data);
      if (payload.request_id && payload.request_id !== currentRequestID) return;
      handleProgressEvent(payload);
    } catch {
      appendStreamMessage({state: "error", message: "Received malformed progress event."});
    }
  });

  socket.addEventListener("close", () => {
    if (currentProgressSocket !== socket) return;
    streamStatus.textContent = "Disconnected";
    currentProgressSocket = null;
  });

  return {ready};
}

function closeProgressSocket() {
  if (!currentProgressSocket) return;
  currentProgressSocket.close();
  currentProgressSocket = null;
}

function resetStream() {
  streamStatus.textContent = "Not connected";
  streamList.innerHTML = "<li>No streamed messages yet.</li>";
}

function handleProgressEvent(event) {
  appendStreamMessage(event);
  if (event.step && event.state) {
    updateProgress(event.step, event.state, progressLabel(event));
  }
  if (event.message) {
    generationStatus.textContent = event.message;
  }
  if (event.type === "complete") {
    streamStatus.textContent = "Completed";
    closeProgressSocket();
  } else if (event.type === "error") {
    streamStatus.textContent = "Error";
  }
}

function progressLabel(event) {
  if (event.state === "active" && event.attempt) return `Attempt ${event.attempt}`;
  if (event.state === "done") return "Done";
  if (event.state === "retry") return "Retrying";
  if (event.state === "error") return "Failed";
  return event.message || event.state;
}

function appendStreamMessage(event) {
  if (!streamList) return;
  if (streamList.children.length === 1 && streamList.children[0].textContent === "No streamed messages yet.") {
    streamList.innerHTML = "";
  }
  const item = document.createElement("li");
  const time = document.createElement("span");
  time.className = "nl-stream-time";
  time.textContent = formatStreamTime(event.timestamp);
  item.appendChild(time);
  if (event.tool) {
    const tool = document.createElement("span");
    tool.className = "nl-stream-tool";
    tool.textContent = event.tool;
    item.appendChild(tool);
  }
  const message = document.createElement("span");
  message.textContent = event.message || event.state || "Progress update";
  item.appendChild(message);
  streamList.appendChild(item);
  streamList.scrollTop = streamList.scrollHeight;
}

function formatStreamTime(timestamp) {
  const date = timestamp ? new Date(timestamp) : new Date();
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString([], {hour: "2-digit", minute: "2-digit", second: "2-digit"});
}

async function copySparql() {
  const query = sparqlOutput.value.trim();
  if (!query) return;

  try {
    await navigator.clipboard.writeText(query);
    generationStatus.textContent = "SPARQL copied.";
  } catch {
    generationStatus.textContent = "Copy failed.";
  }
}

async function runSparql() {
  const validation = validateSparqlForRun(sparqlOutput.value);
  if (!validation.valid) {
    resultsStatus.textContent = `Query not run: ${validation.errors.join("; ")}`;
    updateProgress("results", "error", "Client validation failed");
    return;
  }

  sparqlOutput.value = validation.query;
  runBtn.disabled = true;
  resultsStatus.textContent = "Running query...";
  resultsTableWrap.innerHTML = "";
  updateProgress("results", "active", "Querying endpoint");

  try {
    const body = new URLSearchParams({query: validation.query});
    const response = await fetch(SPARQL_ENDPOINT, {
      method: "POST",
      headers: {
        Accept: "application/sparql-results+json",
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body,
    });

    if (!response.ok) {
      const text = await response.text();
      throw new Error(`HTTP ${response.status}: ${text.slice(0, 300)}`);
    }

    const data = await response.json();
    renderResults(data);
    updateProgress("results", "done", "Completed");
  } catch (error) {
    resultsStatus.textContent = `Query failed: ${error.message}`;
    updateProgress("results", "error", "Failed");
  } finally {
    runBtn.disabled = sparqlOutput.value.trim() === "";
  }
}

function validateSparqlForRun(query) {
  let normalized = query.trim();
  const errors = [];

  if (!normalized) errors.push("SPARQL query is empty");
  if (destructiveQueryPattern.test(normalized)) errors.push("SPARQL update commands are not allowed");

  const match = normalized.match(queryTypePattern);
  if (!match) {
    errors.push("Query must start with PREFIX declarations followed by SELECT, ASK, CONSTRUCT, or DESCRIBE");
  } else if (match[1].toUpperCase() !== "ASK" && !limitPattern.test(normalized)) {
    normalized = `${normalized.replace(/[;\s]+$/g, "")}\nLIMIT ${DEFAULT_LIMIT}`;
  }

  return {valid: errors.length === 0, errors, query: normalized};
}

function renderResults(data) {
  const variables = data?.head?.vars || [];
  const bindings = data?.results?.bindings || [];
  if (!variables.length) {
    resultsStatus.textContent = "Query completed. No tabular variables returned.";
    return;
  }

  const table = document.createElement("table");
  table.className = "nl-results-table";

  const thead = document.createElement("thead");
  const headerRow = document.createElement("tr");
  variables.forEach(variable => {
    const th = document.createElement("th");
    th.textContent = variable;
    headerRow.appendChild(th);
  });
  thead.appendChild(headerRow);
  table.appendChild(thead);

  const tbody = document.createElement("tbody");
  bindings.forEach(binding => {
    const row = document.createElement("tr");
    variables.forEach(variable => {
      const td = document.createElement("td");
      const value = binding[variable];
      renderBinding(td, value);
      row.appendChild(td);
    });
    tbody.appendChild(row);
  });
  table.appendChild(tbody);

  resultsTableWrap.innerHTML = "";
  resultsTableWrap.appendChild(table);
  resultsStatus.textContent = `${bindings.length} result row(s).`;
}

function renderBinding(cell, binding) {
  if (!binding) return;
  if (binding.type === "uri") {
    const link = document.createElement("a");
    link.href = binding.value;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.textContent = binding.value;
    cell.appendChild(link);
    return;
  }
  cell.textContent = binding.value || "";
}

function renderList(element, items) {
  element.innerHTML = "";
  if (!items.length) {
    const item = document.createElement("li");
    item.textContent = "None.";
    element.appendChild(item);
    return;
  }
  items.forEach(text => {
    const item = document.createElement("li");
    item.textContent = text;
    element.appendChild(item);
  });
}

function resetProgress(options = {}) {
  progressEls.forEach((element, step) => {
    if (options.keepBackend && step === "backend" && element.classList.contains("done")) {
      return;
    }
    updateProgress(step, "pending", step === "backend" ? "Not checked" : "Waiting");
  });
}

function updateProgress(step, state, label) {
  const element = progressEls.get(step);
  if (!element) return;
  element.className = state;
  const status = element.querySelector("strong");
  if (status) status.textContent = label;
}

void checkBackend();
