const EXAMPLES_JSON_PATH = "./examples.json";
const ENDPOINT_URL = "https://linked.rism.io/api";
const PAGE_SIZE = 100;

const toggleBtn = document.getElementById("toggleSidebarBtn");
const examplesStatusEl = document.getElementById("examplesStatus");
const listEl = document.getElementById("examplesList");
const yasguiRootEl = document.getElementById("yasgui");
const prevPageBtn = document.getElementById("prevPageBtn");
const nextPageBtn = document.getElementById("nextPageBtn");
const pageIndicatorEl = document.getElementById("pageIndicator");
const queryModeStatusEl = document.getElementById("queryModeStatus");

let yasgui = null;
const pagingState = {
  currentOffset: 0,
  pageSize: PAGE_SIZE,
  managedPagingEnabled: false,
  manualLimitDetected: false,
  lastQueryFingerprint: "",
  lastQueryType: "",
};

toggleBtn.addEventListener("click", () => {
  const isOpen = document.body.classList.contains("sidebar-open");
  document.body.classList.toggle("sidebar-open", !isOpen);
  document.body.classList.toggle("sidebar-collapsed", isOpen);
  toggleBtn.textContent = isOpen ? "Show examples" : "Hide examples";
  toggleBtn.setAttribute("aria-expanded", String(!isOpen));
});

prevPageBtn.addEventListener("click", () => {
  if (!pagingState.managedPagingEnabled || pagingState.currentOffset === 0) return;
  pagingState.currentOffset = Math.max(0, pagingState.currentOffset - pagingState.pageSize);
  updatePagingUi();
  rerunActiveQuery();
});

nextPageBtn.addEventListener("click", () => {
  if (!pagingState.managedPagingEnabled) return;
  pagingState.currentOffset += pagingState.pageSize;
  updatePagingUi();
  rerunActiveQuery();
});

function setExamplesStatus(message) {
  examplesStatusEl.textContent = message;
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
      setExamplesStatus(`Copied: ${example.title}`);
      return;
    }
    const copied = fallbackCopy(example.query);
    setExamplesStatus(copied ? `Copied: ${example.title}` : `Copy failed: ${example.title}`);
  } catch {
    setExamplesStatus(`Copy failed: ${example.title}`);
  }
}

function getActiveYasqe() {
  return yasgui?.getTab?.()?.getYasqe?.() || null;
}

function normalizeQuery(query) {
  return query.replace(/\s+/g, " ").trim();
}

function hasExplicitLimitOrOffset(query) {
  return /\bLIMIT\s+\d+\b/i.test(query) || /\bOFFSET\s+\d+\b/i.test(query);
}

function syncPagingStateFromEditor() {
  const yasqe = getActiveYasqe();
  if (!yasqe) {
    pagingState.managedPagingEnabled = false;
    pagingState.manualLimitDetected = false;
    pagingState.lastQueryType = "";
    updatePagingUi();
    return;
  }

  const rawQuery = yasqe.getValue?.() || "";
  const queryFingerprint = normalizeQuery(rawQuery);
  const queryType = (yasqe.getQueryType?.() || "").toUpperCase();
  const hasManualLimit = hasExplicitLimitOrOffset(rawQuery);

  if (queryFingerprint !== pagingState.lastQueryFingerprint) {
    pagingState.currentOffset = 0;
    pagingState.lastQueryFingerprint = queryFingerprint;
  }

  pagingState.lastQueryType = queryType;
  pagingState.manualLimitDetected = queryType === "SELECT" && hasManualLimit;
  pagingState.managedPagingEnabled = queryType === "SELECT" && !hasManualLimit;
  updatePagingUi();
}

function updatePagingUi() {
  const pageNumber = Math.floor(pagingState.currentOffset / pagingState.pageSize) + 1;
  pageIndicatorEl.textContent = `Page ${pageNumber}`;

  prevPageBtn.disabled = !pagingState.managedPagingEnabled || pagingState.currentOffset === 0;
  nextPageBtn.disabled = !pagingState.managedPagingEnabled;

  if (pagingState.lastQueryType === "SELECT" && pagingState.managedPagingEnabled) {
    queryModeStatusEl.textContent = `POST mode active. Server paging LIMIT ${pagingState.pageSize}, OFFSET ${pagingState.currentOffset}.`;
    return;
  }
  if (pagingState.manualLimitDetected) {
    queryModeStatusEl.textContent = "POST mode active. Manual LIMIT/OFFSET detected; app paging is disabled.";
    return;
  }
  if (pagingState.lastQueryType === "CONSTRUCT") {
    queryModeStatusEl.textContent = `POST mode active. Auto LIMIT ${pagingState.pageSize} applied for CONSTRUCT when missing.`;
    return;
  }
  if (pagingState.lastQueryType) {
    queryModeStatusEl.textContent = "POST mode active.";
    return;
  }
  queryModeStatusEl.textContent = "POST mode active. Waiting for query.";
}

function rewriteQueryForRequest(yasqe) {
  const rawQuery = yasqe.getValue?.() || "";
  const queryType = (yasqe.getQueryType?.() || "").toUpperCase();
  const queryFingerprint = normalizeQuery(rawQuery);
  const hasManualLimit = hasExplicitLimitOrOffset(rawQuery);

  if (queryFingerprint !== pagingState.lastQueryFingerprint) {
    pagingState.currentOffset = 0;
    pagingState.lastQueryFingerprint = queryFingerprint;
  }

  pagingState.lastQueryType = queryType;

  if (queryType === "SELECT") {
    if (hasManualLimit) {
      pagingState.manualLimitDetected = true;
      pagingState.managedPagingEnabled = false;
      updatePagingUi();
      return rawQuery;
    }

    pagingState.manualLimitDetected = false;
    pagingState.managedPagingEnabled = true;
    updatePagingUi();
    return `${rawQuery.trim()}\nLIMIT ${pagingState.pageSize} OFFSET ${pagingState.currentOffset}`;
  }

  if (queryType === "CONSTRUCT") {
    pagingState.manualLimitDetected = false;
    pagingState.managedPagingEnabled = false;
    updatePagingUi();
    if (hasManualLimit) return rawQuery;
    return `${rawQuery.trim()}\nLIMIT ${pagingState.pageSize}`;
  }

  pagingState.manualLimitDetected = false;
  pagingState.managedPagingEnabled = false;
  updatePagingUi();
  return rawQuery;
}

function rerunActiveQuery() {
  const yasqe = getActiveYasqe();
  if (!yasqe || typeof yasqe.query !== "function") return;
  yasqe.query().catch(() => {});
}

function loadQuery(example) {
  if (!yasgui) {
    setExamplesStatus(`Could not load "${example.title}" into the editor.`);
    return;
  }

  pagingState.currentOffset = 0;
  pagingState.lastQueryFingerprint = "";

  const YasguiCtor = window.Yasgui;
  if (
    typeof yasgui.addTab === "function" &&
    YasguiCtor?.Tab &&
    typeof YasguiCtor.Tab.getDefaults === "function"
  ) {
    yasgui.addTab(true, {
      ...YasguiCtor.Tab.getDefaults(),
      name: example.title || "Example",
      requestConfig: {
        ...(yasgui.config?.requestConfig || {}),
        endpoint: ENDPOINT_URL,
      },
      yasqe: {value: example.query},
    });
    syncPagingStateFromEditor();
    setExamplesStatus(`Loaded: ${example.title}`);
    return;
  }

  const yasqe = getActiveYasqe();
  if (yasqe && typeof yasqe.setValue === "function") {
    yasqe.setValue(example.query);
    syncPagingStateFromEditor();
    setExamplesStatus(`Loaded: ${example.title}`);
    return;
  }

  setExamplesStatus(`Could not load "${example.title}" into the editor.`);
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
      setExamplesStatus("No examples found. Add files in examples/ and run scripts/compile-examples.sh.");
      return;
    }
    renderExamples(examples);
    setExamplesStatus(`${examples.length} example(s) loaded.`);
  } catch (error) {
    listEl.innerHTML = "";
    setExamplesStatus(`Failed to load examples (${error.message}). Build examples.json first.`);
  }
}

function bindYasqeChangeHandlers() {
  const tab = yasgui?.getTab?.();
  const yasqe = tab?.getYasqe?.();
  if (!yasqe || typeof yasqe.on !== "function") return;

  yasqe.on("change", () => {
    syncPagingStateFromEditor();
  });
}

function isRenderableImageUrl(value) {
  if (typeof value !== "string") return false;

  try {
    const url = new URL(value);
    if (url.protocol !== "http:" && url.protocol !== "https:") return false;
    const pathname = url.pathname.toLowerCase();
    return [".jpg", ".jpeg", ".png", ".gif", ".webp"].some(extension => pathname.endsWith(extension));
  } catch {
    return false;
  }
}

function renderImageCell(cell, binding) {
  if (!binding || binding.type !== "uri" || !isRenderableImageUrl(binding.value)) return;

  const wrapper = document.createElement("div");
  wrapper.className = "yasr-image-cell";

  const link = document.createElement("a");
  link.className = "yasr-image-link";
  link.href = binding.value;
  link.target = "_blank";
  link.rel = "noopener noreferrer";

  const image = document.createElement("img");
  image.className = "yasr-result-image";
  image.src = binding.value;
  image.alt = binding.value;
  image.loading = "lazy";

  link.appendChild(image);
  wrapper.appendChild(link);
  cell.replaceChildren(wrapper);
}

function initYasgui() {
  if (!window.Yasgui) {
    throw new Error("YASGUI script did not load.");
  }

  if (window.Yasr?.plugins?.table?.defaults?.tableConfig) {
    window.Yasr.plugins.table.defaults.tableConfig = {
      ...window.Yasr.plugins.table.defaults.tableConfig,
      pageLength: PAGE_SIZE,
      lengthChange: false,
      paging: false,
      info: false,
      columnDefs: [
        ...(window.Yasr.plugins.table.defaults.tableConfig.columnDefs || []),
        {
          targets: "_all",
          createdCell: renderImageCell,
        },
      ],
    };
  }

  yasgui = new window.Yasgui(yasguiRootEl, {
    requestConfig: {
      endpoint: ENDPOINT_URL,
      method: "POST",
      adjustQueryBeforeRequest: rewriteQueryForRequest,
    },
    copyEndpointOnNewTab: true,
  });

  bindYasqeChangeHandlers();
  syncPagingStateFromEditor();

  if (typeof yasgui.on === "function") {
    yasgui.on("tabSelect", () => {
      bindYasqeChangeHandlers();
      syncPagingStateFromEditor();
    });
    yasgui.on("tabAdd", () => {
      bindYasqeChangeHandlers();
      syncPagingStateFromEditor();
    });
  }
}

async function init() {
  initYasgui();
  await loadExamples();
}

void init();
