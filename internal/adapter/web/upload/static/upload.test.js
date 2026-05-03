// upload.test.js — node --test unit tests for upload.js (SIN-62258).
// Run with:  node --test internal/adapter/web/upload/static/upload.test.js
//
// We exercise the pure helpers (detectFormat, classify, messageForStatus,
// messageForCode, formatBytes, policyForForm). Browser-specific paths
// (HTMX events, drawPreview, FileReader / XMLHttpRequest plumbing) are
// covered by the future Playwright suite (SIN-62258 E2E child issue).

const test = require("node:test");
const assert = require("node:assert");

const SinUpload = require("./upload.js");

// ---- detectFormat -----------------------------------------------------

test("detectFormat: PNG magic bytes", () => {
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  assert.equal(SinUpload.detectFormat(png), "png");
});

test("detectFormat: JPEG magic bytes", () => {
  const jpeg = new Uint8Array([0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10]);
  assert.equal(SinUpload.detectFormat(jpeg), "jpeg");
});

test("detectFormat: WEBP magic bytes (RIFF...WEBP)", () => {
  const webp = new Uint8Array([
    0x52, 0x49, 0x46, 0x46, // "RIFF"
    0x00, 0x00, 0x00, 0x00, // size placeholder
    0x57, 0x45, 0x42, 0x50, // "WEBP"
  ]);
  assert.equal(SinUpload.detectFormat(webp), "webp");
});

test("detectFormat: PDF magic bytes (%PDF-)", () => {
  const pdf = new Uint8Array([0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e]);
  assert.equal(SinUpload.detectFormat(pdf), "pdf");
});

test("detectFormat: SVG / text content rejected", () => {
  // "<?xml " — leading bytes of a real SVG. MUST NOT pass any sniff.
  const svgXML = new Uint8Array([0x3c, 0x3f, 0x78, 0x6d, 0x6c, 0x20]);
  assert.equal(SinUpload.detectFormat(svgXML), "");
  // "<svg "
  const svgInline = new Uint8Array([0x3c, 0x73, 0x76, 0x67, 0x20]);
  assert.equal(SinUpload.detectFormat(svgInline), "");
});

test("detectFormat: Windows PE (.exe) rejected", () => {
  // "MZ" header — classic Windows EXE magic.
  const exe = new Uint8Array([
    0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00,
  ]);
  assert.equal(SinUpload.detectFormat(exe), "");
});

test("detectFormat: empty / short buffer", () => {
  assert.equal(SinUpload.detectFormat(null), "");
  assert.equal(SinUpload.detectFormat(undefined), "");
  assert.equal(SinUpload.detectFormat(new Uint8Array([])), "");
  assert.equal(SinUpload.detectFormat(new Uint8Array([0x89])), "");
});

test("detectFormat: WEBP with wrong tag rejected", () => {
  // RIFF...XYZP — not WEBP.
  const wrong = new Uint8Array([
    0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 0x58, 0x59, 0x5a, 0x50,
  ]);
  assert.equal(SinUpload.detectFormat(wrong), "");
});

// ---- formatBytes ------------------------------------------------------

test("formatBytes: 2MB / 20MB / 1MB / 1.5MB / 512KB", () => {
  assert.equal(SinUpload.formatBytes(2 * 1024 * 1024), "2MB");
  assert.equal(SinUpload.formatBytes(20 * 1024 * 1024), "20MB");
  assert.equal(SinUpload.formatBytes(1024 * 1024), "1MB");
  assert.equal(SinUpload.formatBytes(1500 * 1024), "1.5MB");
  assert.equal(SinUpload.formatBytes(512 * 1024), "512KB");
});

// ---- messageForStatus -------------------------------------------------

test("messageForStatus: 413 with limit produces sized PT-BR message", () => {
  assert.equal(
    SinUpload.messageForStatus(413, { maxBytes: 2 * 1024 * 1024 }),
    "Arquivo muito grande. Limite: 2MB.",
  );
  assert.equal(
    SinUpload.messageForStatus(413, { maxBytes: 20 * 1024 * 1024 }),
    "Arquivo muito grande. Limite: 20MB.",
  );
});

test("messageForStatus: 413 without ctx falls back to short PT-BR", () => {
  assert.equal(SinUpload.messageForStatus(413), "Arquivo muito grande.");
});

test("messageForStatus: 415 / 429 / 0 / unknown", () => {
  assert.equal(SinUpload.messageForStatus(415), SinUpload.MSG_SERVER_REJECTED);
  assert.equal(SinUpload.messageForStatus(429), SinUpload.MSG_RATE_LIMITED);
  assert.equal(SinUpload.messageForStatus(0), SinUpload.MSG_NETWORK);
  assert.equal(SinUpload.messageForStatus(500), SinUpload.MSG_UNKNOWN);
  assert.equal(SinUpload.messageForStatus(401), SinUpload.MSG_UNKNOWN);
});

// ---- messageForCode ---------------------------------------------------

test("messageForCode: decompression_bomb wins over status", () => {
  assert.equal(
    SinUpload.messageForCode("decompression_bomb", 400),
    SinUpload.MSG_DECOMPRESSION_BOMB,
  );
  assert.equal(
    SinUpload.messageForCode("decompression_bomb", 422),
    SinUpload.MSG_DECOMPRESSION_BOMB,
  );
});

test("messageForCode: unknown code falls through to messageForStatus", () => {
  assert.equal(
    SinUpload.messageForCode("weird", 415),
    SinUpload.MSG_SERVER_REJECTED,
  );
  assert.equal(
    SinUpload.messageForCode("", 413, { maxBytes: 2 * 1024 * 1024 }),
    "Arquivo muito grande. Limite: 2MB.",
  );
});

// ---- classify (with stub File) ----------------------------------------

// Minimal File stub: { size, slice() returns Blob-like with same arrayBuffer.}
function fakeFile(bytes, opts) {
  const buf = bytes.buffer || bytes;
  const totalSize = (opts && typeof opts.size === "number") ? opts.size : bytes.length;
  return {
    size: totalSize,
    slice(start, end) {
      // Slice the prefix only (we never need more than 12 bytes).
      const view = new Uint8Array(buf, 0, Math.min(end || bytes.length, bytes.length));
      return { _bytes: view };
    },
  };
}

// Replace global FileReader with a stub that resolves arrayBuffer with the
// stubbed slice's _bytes.
class FakeFileReader {
  constructor() {
    this.onload = null;
    this.onerror = null;
    this.result = null;
  }
  readAsArrayBuffer(blob) {
    setImmediate(() => {
      if (!blob || !blob._bytes) {
        if (this.onerror) this.onerror();
        return;
      }
      this.result = blob._bytes.buffer.slice(
        blob._bytes.byteOffset,
        blob._bytes.byteOffset + blob._bytes.byteLength,
      );
      if (this.onload) this.onload();
    });
  }
}

test("classify: PNG accepted by logo policy", async () => {
  global.FileReader = FakeFileReader;
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0]);
  const res = await SinUpload.classify(fakeFile(png), {
    allowed: ["png", "jpeg", "webp"],
    maxBytes: 2 * 1024 * 1024,
  });
  assert.equal(res.ok, true);
  assert.equal(res.format, "png");
  assert.equal(res.message, "");
});

test("classify: PNG rejected by attachment-only policy when png not in allowed", async () => {
  global.FileReader = FakeFileReader;
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0]);
  const res = await SinUpload.classify(fakeFile(png), {
    allowed: ["pdf"],
    maxBytes: 20 * 1024 * 1024,
    unsupportedMessage: SinUpload.MSG_UNSUPPORTED_ATTACHMENT,
  });
  assert.equal(res.ok, false);
  assert.equal(res.message, SinUpload.MSG_UNSUPPORTED_ATTACHMENT);
});

test("classify: SVG rejected (defense-in-depth — no SVG-as-blob)", async () => {
  global.FileReader = FakeFileReader;
  const svg = new Uint8Array([0x3c, 0x73, 0x76, 0x67, 0x20, 0x78, 0x6d, 0x6c, 0x6e, 0x73, 0x3d, 0x22]);
  const res = await SinUpload.classify(fakeFile(svg), {
    allowed: ["png", "jpeg", "webp"],
    maxBytes: 2 * 1024 * 1024,
    unsupportedMessage: SinUpload.MSG_UNSUPPORTED_LOGO,
  });
  assert.equal(res.ok, false);
  assert.equal(res.message, SinUpload.MSG_UNSUPPORTED_LOGO);
});

test("classify: PNG with .svg extension accepted (magic byte > extension)", async () => {
  global.FileReader = FakeFileReader;
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0]);
  const fake = fakeFile(png);
  fake.name = "fake-logo.svg";
  const res = await SinUpload.classify(fake, {
    allowed: ["png", "jpeg", "webp"],
    maxBytes: 2 * 1024 * 1024,
  });
  assert.equal(res.ok, true);
  assert.equal(res.format, "png");
});

test("classify: EXE renamed .png rejected", async () => {
  global.FileReader = FakeFileReader;
  const exe = new Uint8Array([
    0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00,
  ]);
  const fake = fakeFile(exe);
  fake.name = "evil.png";
  const res = await SinUpload.classify(fake, {
    allowed: ["png", "jpeg", "webp"],
    maxBytes: 2 * 1024 * 1024,
    unsupportedMessage: SinUpload.MSG_UNSUPPORTED_LOGO,
  });
  assert.equal(res.ok, false);
  assert.equal(res.message, SinUpload.MSG_UNSUPPORTED_LOGO);
});

test("classify: too-large file blocked before read", async () => {
  global.FileReader = FakeFileReader;
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0]);
  const big = fakeFile(png, { size: 5 * 1024 * 1024 });
  const res = await SinUpload.classify(big, {
    allowed: ["png", "jpeg", "webp"],
    maxBytes: 2 * 1024 * 1024,
  });
  assert.equal(res.ok, false);
  assert.equal(res.message, "Arquivo muito grande. Limite: 2MB.");
});

test("classify: null file produces friendly error", async () => {
  const res = await SinUpload.classify(null, {
    allowed: ["png"],
    maxBytes: 2 * 1024 * 1024,
  });
  assert.equal(res.ok, false);
  assert.equal(res.message, SinUpload.MSG_UNKNOWN);
});

test("classify: read error surfaces MSG_UNKNOWN", async () => {
  // FileReader stub that always errors.
  global.FileReader = class {
    constructor() {
      this.onload = null;
      this.onerror = null;
    }
    readAsArrayBuffer() {
      setImmediate(() => {
        if (this.onerror) this.onerror();
      });
    }
  };
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0]);
  const res = await SinUpload.classify(fakeFile(png), {
    allowed: ["png"],
    maxBytes: 2 * 1024 * 1024,
  });
  assert.equal(res.ok, false);
  assert.equal(res.message, SinUpload.MSG_UNKNOWN);
});

// ---- policyForForm ----------------------------------------------------

test("policyForForm: logo defaults", () => {
  const fakeForm = {
    _attrs: { "data-upload": "logo" },
    getAttribute(k) {
      return Object.prototype.hasOwnProperty.call(this._attrs, k)
        ? this._attrs[k]
        : null;
    },
  };
  const p = SinUpload.policyForForm(fakeForm);
  assert.equal(p.kind, "logo");
  assert.deepEqual(p.allowed, ["png", "jpeg", "webp"]);
  assert.equal(p.maxBytes, 2 * 1024 * 1024);
  assert.equal(p.unsupportedMessage, SinUpload.MSG_UNSUPPORTED_LOGO);
});

test("policyForForm: attachment defaults", () => {
  const fakeForm = {
    _attrs: { "data-upload": "attachment" },
    getAttribute(k) {
      return Object.prototype.hasOwnProperty.call(this._attrs, k)
        ? this._attrs[k]
        : null;
    },
  };
  const p = SinUpload.policyForForm(fakeForm);
  assert.equal(p.kind, "attachment");
  assert.deepEqual(p.allowed, ["png", "jpeg", "webp", "pdf"]);
  assert.equal(p.maxBytes, 20 * 1024 * 1024);
  assert.equal(p.unsupportedMessage, SinUpload.MSG_UNSUPPORTED_ATTACHMENT);
});

test("policyForForm: data-* overrides applied", () => {
  const fakeForm = {
    _attrs: {
      "data-upload": "logo",
      "data-upload-allowed": "png, webp",
      "data-upload-max-bytes": "1048576",
    },
    getAttribute(k) {
      return Object.prototype.hasOwnProperty.call(this._attrs, k)
        ? this._attrs[k]
        : null;
    },
  };
  const p = SinUpload.policyForForm(fakeForm);
  assert.deepEqual(p.allowed, ["png", "webp"]);
  assert.equal(p.maxBytes, 1024 * 1024);
});

// ---- setError / setProgress (jsdom-free DOM stubs) --------------------

function fakeNode() {
  return {
    _attrs: {},
    _children: [],
    _hidden: false,
    textContent: "",
    setAttribute(k, v) {
      this._attrs[k] = v;
      if (k === "hidden") this._hidden = true;
    },
    removeAttribute(k) {
      delete this._attrs[k];
      if (k === "hidden") this._hidden = false;
    },
    getAttribute(k) {
      return Object.prototype.hasOwnProperty.call(this._attrs, k)
        ? this._attrs[k]
        : null;
    },
    querySelector(sel) {
      return this._children.find((c) => c.matches && c.matches(sel)) || null;
    },
  };
}

function makeFakeForm() {
  const errorNode = fakeNode();
  errorNode.matches = (s) => s === "[data-upload-error]";
  const wrap = fakeNode();
  wrap.matches = (s) => s === "[data-upload-progress]";
  const bar = fakeNode();
  bar.matches = (s) => s === "progress[data-upload-bar]";
  bar.value = 0;
  bar.max = 100;
  const form = fakeNode();
  form._children = [errorNode, wrap, bar];
  return { form, errorNode, wrap, bar };
}

test("setError: writes msg + clears hidden when non-empty", () => {
  const { form, errorNode } = makeFakeForm();
  errorNode.setAttribute("hidden", "");
  SinUpload.setError(form, "boom");
  assert.equal(errorNode.textContent, "boom");
  assert.equal(errorNode._hidden, false);
});

test("setError: empty msg sets hidden", () => {
  const { form, errorNode } = makeFakeForm();
  SinUpload.setError(form, "");
  assert.equal(errorNode.textContent, "");
  assert.equal(errorNode._hidden, true);
});

test("setError: missing slot is a no-op", () => {
  const form = fakeNode();
  SinUpload.setError(form, "msg"); // must not throw
});

test("setProgress: shows bar mid-flight, hides at completion", () => {
  const { form, wrap, bar } = makeFakeForm();
  SinUpload.setProgress(form, 50, 100);
  assert.equal(bar.value, 50);
  assert.equal(bar.max, 100);
  assert.equal(wrap._hidden, false);

  SinUpload.setProgress(form, 100, 100);
  assert.equal(wrap._hidden, true);
});

test("setProgress: total=0 hides indicator", () => {
  const { form, wrap } = makeFakeForm();
  SinUpload.setProgress(form, 0, 0);
  assert.equal(wrap._hidden, true);
});

// ---- bind / init coverage --------------------------------------------
//
// bind() registers event listeners on the form for the htmx lifecycle.
// We build a richer fake form that mimics the EventTarget shape so we
// can dispatch synthetic events and assert the side effects on the
// error/progress regions.

function fakeUploadForm(kind) {
  kind = kind || "logo";
  const errorNode = fakeNode();
  errorNode.matches = (s) => s === "[data-upload-error]";
  const wrap = fakeNode();
  wrap.matches = (s) => s === "[data-upload-progress]";
  const bar = fakeNode();
  bar.matches = (s) => s === "progress[data-upload-bar]";
  bar.value = 0;
  bar.max = 100;

  const cancelBtn = fakeNode();
  cancelBtn.matches = (s) => s === "[data-upload-cancel]";
  cancelBtn._listeners = {};
  cancelBtn.addEventListener = function (type, fn) {
    cancelBtn._listeners[type] = (cancelBtn._listeners[type] || []).concat([fn]);
  };
  cancelBtn.dispatch = function (type, ev) {
    (cancelBtn._listeners[type] || []).forEach((fn) => fn(ev));
  };

  const fileInput = fakeNode();
  fileInput.matches = (s) => s === 'input[type="file"]';
  fileInput.type = "file";
  fileInput.files = [];
  fileInput.value = "";
  fileInput.focus = () => {
    fileInput._focused = true;
  };
  fileInput._listeners = {};
  fileInput.addEventListener = function (type, fn) {
    fileInput._listeners[type] = (fileInput._listeners[type] || []).concat([
      fn,
    ]);
  };
  fileInput.dispatch = function (type, ev) {
    (fileInput._listeners[type] || []).forEach((fn) => fn(ev));
  };

  const canvas = fakeNode();
  canvas.matches = (s) => s === "canvas[data-upload-preview]";
  canvas.getContext = () => null; // skip preview drawing in tests

  const form = fakeNode();
  form._attrs["data-upload"] = kind;
  form._children = [fileInput, errorNode, wrap, bar, cancelBtn, canvas];
  form._listeners = {};
  form.addEventListener = function (type, fn) {
    form._listeners[type] = (form._listeners[type] || []).concat([fn]);
  };
  form.dispatch = function (type, ev) {
    (form._listeners[type] || []).forEach((fn) => fn(ev));
  };
  return { form, fileInput, errorNode, wrap, bar, cancelBtn };
}

test("bind: htmx:responseError with 415 sets MSG_SERVER_REJECTED", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:responseError", {
    detail: { xhr: { status: 415, responseText: "" } },
  });
  assert.equal(errorNode.textContent, SinUpload.MSG_SERVER_REJECTED);
  assert.equal(errorNode._hidden, false);
});

test("bind: htmx:responseError with 429 sets MSG_RATE_LIMITED", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:responseError", {
    detail: { xhr: { status: 429, responseText: "{}" } },
  });
  assert.equal(errorNode.textContent, SinUpload.MSG_RATE_LIMITED);
});

test("bind: htmx:responseError with 413 uses policy maxBytes", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:responseError", {
    detail: { xhr: { status: 413, responseText: "" } },
  });
  assert.equal(errorNode.textContent, "Arquivo muito grande. Limite: 2MB.");
});

test("bind: htmx:responseError with decompression_bomb code wins over status", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:responseError", {
    detail: {
      xhr: { status: 400, responseText: '{"code":"decompression_bomb"}' },
    },
  });
  assert.equal(errorNode.textContent, SinUpload.MSG_DECOMPRESSION_BOMB);
});

test("bind: htmx:responseError with malformed JSON falls back to status mapping", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:responseError", {
    detail: { xhr: { status: 415, responseText: "not-json{" } },
  });
  assert.equal(errorNode.textContent, SinUpload.MSG_SERVER_REJECTED);
});

test("bind: htmx:responseError with no xhr surfaces MSG_UNKNOWN", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:responseError", {});
  assert.equal(errorNode.textContent, SinUpload.MSG_UNKNOWN);
});

test("bind: htmx:sendError surfaces MSG_NETWORK", () => {
  const { form, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:sendError", {});
  assert.equal(errorNode.textContent, SinUpload.MSG_NETWORK);
});

test("bind: htmx:xhr:progress drives the <progress> bar", () => {
  const { form, bar, wrap } = fakeUploadForm("logo");
  SinUpload.bind(form);
  form.dispatch("htmx:xhr:progress", { detail: { loaded: 50, total: 100 } });
  assert.equal(bar.value, 50);
  assert.equal(bar.max, 100);
  assert.equal(wrap._hidden, false);
});

test("bind: htmx:beforeOnLoad clears progress", () => {
  const { form, wrap } = fakeUploadForm("logo");
  SinUpload.bind(form);
  // Populate first, then clear.
  form.dispatch("htmx:xhr:progress", { detail: { loaded: 50, total: 100 } });
  form.dispatch("htmx:beforeOnLoad", {});
  assert.equal(wrap._hidden, true);
});

test("bind: htmx:beforeRequest blocks too-large file", () => {
  const { form, fileInput, errorNode } = fakeUploadForm("logo");
  fileInput.files = [{ size: 5 * 1024 * 1024 }];
  let prevented = false;
  SinUpload.bind(form);
  form.dispatch("htmx:beforeRequest", {
    preventDefault() {
      prevented = true;
    },
  });
  assert.equal(prevented, true);
  assert.equal(errorNode.textContent, "Arquivo muito grande. Limite: 2MB.");
});

test("bind: htmx:beforeRequest no-op when no files", () => {
  const { form } = fakeUploadForm("logo");
  SinUpload.bind(form);
  let prevented = false;
  form.dispatch("htmx:beforeRequest", {
    preventDefault() {
      prevented = true;
    },
  });
  assert.equal(prevented, false);
});

test("bind: input change with PNG drawPreview is exercised (no canvas ctx)", async () => {
  global.FileReader = FakeFileReader;
  const { form, fileInput, errorNode } = fakeUploadForm("logo");
  const png = new Uint8Array([
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0,
  ]);
  const fake = fakeFile(png);
  fileInput.files = [fake];
  SinUpload.bind(form);
  fileInput.dispatch("change", {});
  // Wait two microtasks for the FileReader+then chain.
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.equal(errorNode.textContent, "");
});

test("bind: input change with SVG sets unsupported PT-BR error and clears value", async () => {
  global.FileReader = FakeFileReader;
  const { form, fileInput, errorNode } = fakeUploadForm("logo");
  const svg = new Uint8Array([
    0x3c, 0x73, 0x76, 0x67, 0x20, 0x78, 0x6d, 0x6c, 0x6e, 0x73, 0x3d, 0x22,
  ]);
  const fake = fakeFile(svg);
  fileInput.files = [fake];
  fileInput.value = "logo.svg";
  SinUpload.bind(form);
  fileInput.dispatch("change", {});
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.equal(errorNode.textContent, SinUpload.MSG_UNSUPPORTED_LOGO);
  assert.equal(fileInput.value, "");
});

test("bind: cancel button aborts and surfaces MSG_CANCELLED", () => {
  const { form, fileInput, cancelBtn, errorNode } = fakeUploadForm("logo");
  SinUpload.bind(form);
  let prevented = false;
  cancelBtn.dispatch("click", {
    preventDefault() {
      prevented = true;
    },
  });
  assert.equal(prevented, true);
  assert.equal(errorNode.textContent, SinUpload.MSG_CANCELLED);
  assert.equal(fileInput._focused, true);
});

test("bind: idempotent — second bind is a no-op", () => {
  const { form } = fakeUploadForm("logo");
  SinUpload.bind(form);
  const before = (form._listeners["htmx:responseError"] || []).length;
  SinUpload.bind(form);
  const after = (form._listeners["htmx:responseError"] || []).length;
  assert.equal(after, before);
});

test("bind: null is a no-op", () => {
  SinUpload.bind(null); // must not throw
});

test("init: binds all forms in scope", () => {
  const { form: f1 } = fakeUploadForm("logo");
  const { form: f2 } = fakeUploadForm("attachment");
  const scope = {
    querySelectorAll(sel) {
      return sel === "form[data-upload]" ? [f1, f2] : [];
    },
  };
  SinUpload.init(scope);
  assert.ok(f1.__sinUploadBound);
  assert.ok(f2.__sinUploadBound);
});
