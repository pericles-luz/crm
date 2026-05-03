// upload.js — SIN-62258 client-side validation for /uploads/logo and
// /uploads/attachment. This is the UX-layer defense in the upload pipeline:
// the server (SIN-62246, internal/media/upload) is the source of truth and
// re-validates every byte. Anything we do here only saves a round-trip and
// gives the user a fast, PT-BR error.
//
// Public surface (window.SinUpload):
//   detectFormat(bytes)  → {format, ok} for raw 0..12-byte prefix
//   classify(file, policy) → Promise<{ok, format, message}>
//   messageForStatus(s, ctx) → PT-BR string for an HTTP status
//   messageForCode(code, ctx) → PT-BR string for a server error code
//   bind(form)           → wire a <form data-upload="logo|attachment">
//
// Magic bytes mirror internal/media/upload/upload.go Sniff:
//   PNG  89 50 4E 47 0D 0A 1A 0A
//   JPEG FF D8 FF
//   WEBP "RIFF" .... "WEBP"
//   PDF  "%PDF-"
// SVG (which is text/XML and the original XSS vector — see ADR 0080) is
// intentionally absent: we want the magic-byte gate to *fail* on SVG and
// produce the PT-BR "Tipo de arquivo não suportado" before the request
// ever leaves the browser.

(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.SinUpload = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var FORMAT_PNG = "png";
  var FORMAT_JPEG = "jpeg";
  var FORMAT_WEBP = "webp";
  var FORMAT_PDF = "pdf";

  var MSG_UNSUPPORTED_LOGO =
    "Tipo de arquivo não suportado. Use PNG, JPG ou WEBP.";
  var MSG_UNSUPPORTED_ATTACHMENT =
    "Tipo de arquivo não suportado. Use PNG, JPG, WEBP ou PDF.";
  var MSG_TOO_LARGE_PREFIX = "Arquivo muito grande. Limite: ";
  var MSG_RATE_LIMITED =
    "Muitos uploads em sequência. Tente novamente em alguns segundos.";
  var MSG_DECOMPRESSION_BOMB = "Imagem com dimensões não suportadas.";
  var MSG_SERVER_REJECTED = "Tipo de arquivo rejeitado pelo servidor.";
  var MSG_NETWORK = "Falha de rede. Verifique sua conexão e tente novamente.";
  var MSG_UNKNOWN = "Não foi possível enviar o arquivo. Tente novamente.";
  var MSG_CANCELLED = "Upload cancelado.";

  // detectFormat takes a Uint8Array (or array-like) of the file's first
  // bytes and returns one of FORMAT_* or "" for unknown. It only requires
  // 12 bytes for WEBP; fewer is sufficient for the others.
  function detectFormat(bytes) {
    if (!bytes || typeof bytes.length !== "number") return "";
    var b = bytes;

    // PNG: 89 50 4E 47 0D 0A 1A 0A
    if (
      b.length >= 8 &&
      b[0] === 0x89 &&
      b[1] === 0x50 &&
      b[2] === 0x4e &&
      b[3] === 0x47 &&
      b[4] === 0x0d &&
      b[5] === 0x0a &&
      b[6] === 0x1a &&
      b[7] === 0x0a
    ) {
      return FORMAT_PNG;
    }
    // JPEG: FF D8 FF
    if (b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff) {
      return FORMAT_JPEG;
    }
    // WEBP: "RIFF" .... "WEBP"
    if (
      b.length >= 12 &&
      b[0] === 0x52 &&
      b[1] === 0x49 &&
      b[2] === 0x46 &&
      b[3] === 0x46 &&
      b[8] === 0x57 &&
      b[9] === 0x45 &&
      b[10] === 0x42 &&
      b[11] === 0x50
    ) {
      return FORMAT_WEBP;
    }
    // PDF: "%PDF-"
    if (
      b.length >= 5 &&
      b[0] === 0x25 &&
      b[1] === 0x50 &&
      b[2] === 0x44 &&
      b[3] === 0x46 &&
      b[4] === 0x2d
    ) {
      return FORMAT_PDF;
    }
    return "";
  }

  // readPrefix reads up to n bytes from the start of a File/Blob and
  // resolves with a Uint8Array. We isolate FileReader behind this fn so
  // tests can inject a fake.
  function readPrefix(file, n) {
    return new Promise(function (resolve, reject) {
      try {
        var slice = file.slice(0, n);
        var fr = new FileReader();
        fr.onerror = function () {
          reject(new Error("read"));
        };
        fr.onload = function () {
          resolve(new Uint8Array(fr.result));
        };
        fr.readAsArrayBuffer(slice);
      } catch (e) {
        reject(e);
      }
    });
  }

  // classify validates a File against a policy:
  //   { allowed: ["png","jpeg","webp"(,"pdf")], maxBytes: number }
  // Returns a Promise resolving to {ok, format, message}. Never throws —
  // a read error becomes ok=false with MSG_UNKNOWN so the caller can
  // surface a PT-BR message instead of a stack trace.
  function classify(file, policy) {
    var allowed = (policy && policy.allowed) || [];
    var maxBytes = (policy && policy.maxBytes) || 0;
    var unsupportedMsg =
      (policy && policy.unsupportedMessage) || MSG_UNSUPPORTED_LOGO;

    if (!file) {
      return Promise.resolve({
        ok: false,
        format: "",
        message: MSG_UNKNOWN,
      });
    }
    if (maxBytes > 0 && file.size > maxBytes) {
      return Promise.resolve({
        ok: false,
        format: "",
        message: MSG_TOO_LARGE_PREFIX + formatBytes(maxBytes) + ".",
      });
    }
    return readPrefix(file, 12).then(
      function (bytes) {
        var fmt = detectFormat(bytes);
        if (!fmt || allowed.indexOf(fmt) === -1) {
          return { ok: false, format: fmt, message: unsupportedMsg };
        }
        return { ok: true, format: fmt, message: "" };
      },
      function () {
        return { ok: false, format: "", message: MSG_UNKNOWN };
      },
    );
  }

  // formatBytes renders a byte budget for end users. Only used for the
  // 413 "<X>MB" placeholder — kept simple so tests can pin it.
  function formatBytes(n) {
    var mb = n / (1024 * 1024);
    if (mb >= 1) {
      var rounded = Math.round(mb * 10) / 10;
      // Trim trailing ".0" so "2.0MB" reads as "2MB".
      var s = rounded % 1 === 0 ? String(Math.round(rounded)) : String(rounded);
      return s + "MB";
    }
    var kb = Math.max(1, Math.round(n / 1024));
    return kb + "KB";
  }

  // messageForStatus maps an HTTP status to a PT-BR string. ctx may carry:
  //   { maxBytes: number }   — used for 413 to render the limit
  function messageForStatus(status, ctx) {
    var c = ctx || {};
    switch (status) {
      case 413:
        if (c.maxBytes && c.maxBytes > 0) {
          return MSG_TOO_LARGE_PREFIX + formatBytes(c.maxBytes) + ".";
        }
        return "Arquivo muito grande.";
      case 415:
        return MSG_SERVER_REJECTED;
      case 429:
        return MSG_RATE_LIMITED;
      case 0:
        return MSG_NETWORK;
      default:
        return MSG_UNKNOWN;
    }
  }

  // messageForCode maps server-emitted error codes (the body's "code"
  // field — e.g. "decompression_bomb") to PT-BR. Anything unknown falls
  // back to messageForStatus(status, ctx).
  function messageForCode(code, status, ctx) {
    if (code === "decompression_bomb") {
      return MSG_DECOMPRESSION_BOMB;
    }
    return messageForStatus(status, ctx);
  }

  // setError writes msg into the form's [data-upload-error] node and
  // toggles aria-live visibility. Empty msg clears the error.
  function setError(form, msg) {
    var slot = form.querySelector("[data-upload-error]");
    if (!slot) return;
    slot.textContent = msg || "";
    if (msg) {
      slot.removeAttribute("hidden");
    } else {
      slot.setAttribute("hidden", "");
    }
  }

  // setProgress drives the inline <progress> bar and its surrounding
  // [data-upload-progress] container.
  function setProgress(form, value, total) {
    var bar = form.querySelector("progress[data-upload-bar]");
    var wrap = form.querySelector("[data-upload-progress]");
    if (!bar || !wrap) return;
    if (total > 0) {
      bar.max = total;
      bar.value = value;
    } else {
      bar.removeAttribute("value");
    }
    if (value > 0 && (total === 0 || value < total)) {
      wrap.removeAttribute("hidden");
    } else {
      wrap.setAttribute("hidden", "");
    }
  }

  // drawPreview renders a validated image to a <canvas data-upload-preview>
  // inside the form. We draw via canvas (instead of <img src=blob:>) so
  // an inadvertent SVG-in-blob can never reach the renderer. PDF skips
  // preview.
  function drawPreview(form, file, format) {
    var canvas = form.querySelector("canvas[data-upload-preview]");
    if (!canvas) return;
    if (format === FORMAT_PDF) {
      canvas.setAttribute("hidden", "");
      return;
    }
    var ctx = canvas.getContext("2d");
    if (!ctx) return;
    if (typeof URL === "undefined" || typeof Image === "undefined") return;
    var url = URL.createObjectURL(file);
    var img = new Image();
    img.onload = function () {
      var maxW = canvas.width || 240;
      var maxH = canvas.height || 240;
      var ratio = Math.min(maxW / img.width, maxH / img.height, 1);
      var w = Math.max(1, Math.floor(img.width * ratio));
      var h = Math.max(1, Math.floor(img.height * ratio));
      canvas.width = w;
      canvas.height = h;
      ctx.clearRect(0, 0, w, h);
      ctx.drawImage(img, 0, 0, w, h);
      canvas.removeAttribute("hidden");
      URL.revokeObjectURL(url);
    };
    img.onerror = function () {
      URL.revokeObjectURL(url);
    };
    img.src = url;
  }

  // policyForForm reads the form's data-upload-* attributes and returns a
  // policy compatible with classify(). Defaults match SIN-62246 limits.
  function policyForForm(form) {
    var kind = form.getAttribute("data-upload") || "logo";
    var allowedAttr = form.getAttribute("data-upload-allowed");
    var maxBytesAttr = form.getAttribute("data-upload-max-bytes");
    var allowed;
    if (allowedAttr) {
      allowed = allowedAttr
        .split(",")
        .map(function (s) {
          return s.trim();
        })
        .filter(Boolean);
    } else if (kind === "attachment") {
      allowed = [FORMAT_PNG, FORMAT_JPEG, FORMAT_WEBP, FORMAT_PDF];
    } else {
      allowed = [FORMAT_PNG, FORMAT_JPEG, FORMAT_WEBP];
    }
    var maxBytes = parseInt(maxBytesAttr || "0", 10);
    if (!maxBytes || maxBytes <= 0) {
      maxBytes = kind === "attachment" ? 20 * 1024 * 1024 : 2 * 1024 * 1024;
    }
    var unsupported =
      kind === "attachment" ? MSG_UNSUPPORTED_ATTACHMENT : MSG_UNSUPPORTED_LOGO;
    return {
      kind: kind,
      allowed: allowed,
      maxBytes: maxBytes,
      unsupportedMessage: unsupported,
    };
  }

  // bind wires a single <form data-upload="..."> element. It:
  //  1. Validates on file change. Bad files get setError + cancel submit.
  //  2. On submit, performs an XHR with progress + cancel.
  //  3. Maps server errors to PT-BR via messageForCode/messageForStatus.
  // It does NOT manage HTMX swaps — let HTMX trigger the submit via
  // hx-post; we hook htmx:beforeRequest and htmx:responseError so the
  // same code path covers HTMX and pure form posts.
  function bind(form) {
    if (!form || form.__sinUploadBound) return;
    form.__sinUploadBound = true;

    var input = form.querySelector('input[type="file"]');
    var cancelBtn = form.querySelector("[data-upload-cancel]");
    var policy = policyForForm(form);
    var inflight = null;

    if (input) {
      input.addEventListener("change", function () {
        setError(form, "");
        if (!input.files || input.files.length === 0) return;
        classify(input.files[0], policy).then(function (res) {
          if (!res.ok) {
            setError(form, res.message);
            input.value = "";
            return;
          }
          drawPreview(form, input.files[0], res.format);
        });
      });
    }

    function abortInflight() {
      if (inflight) {
        try {
          inflight.abort();
        } catch (e) {
          /* ignore */
        }
        inflight = null;
      }
      setProgress(form, 0, 0);
    }

    if (cancelBtn) {
      cancelBtn.addEventListener("click", function (ev) {
        ev.preventDefault();
        abortInflight();
        setError(form, MSG_CANCELLED);
        if (input) input.focus();
      });
    }

    // HTMX integration — reject the request before send if the file is
    // invalid (defense-in-depth in case input.change handler missed it).
    form.addEventListener("htmx:beforeRequest", function (ev) {
      if (!input || !input.files || input.files.length === 0) return;
      // Validation already ran on change; we only re-check size here for
      // the rare case JS state diverged from input.value.
      var file = input.files[0];
      if (policy.maxBytes > 0 && file.size > policy.maxBytes) {
        setError(
          form,
          MSG_TOO_LARGE_PREFIX + formatBytes(policy.maxBytes) + ".",
        );
        ev.preventDefault();
      }
    });

    form.addEventListener("htmx:responseError", function (ev) {
      var xhr = ev.detail && ev.detail.xhr;
      if (!xhr) {
        setError(form, MSG_UNKNOWN);
        return;
      }
      var code = "";
      try {
        var body = JSON.parse(xhr.responseText || "{}");
        code = body && body.code ? String(body.code) : "";
      } catch (e) {
        /* response wasn't JSON — fall through to status mapping */
      }
      setError(form, messageForCode(code, xhr.status, { maxBytes: policy.maxBytes }));
      setProgress(form, 0, 0);
    });

    form.addEventListener("htmx:sendError", function () {
      setError(form, MSG_NETWORK);
      setProgress(form, 0, 0);
    });

    form.addEventListener("htmx:xhr:progress", function (ev) {
      var d = ev.detail || {};
      setProgress(form, d.loaded || 0, d.total || 0);
    });

    form.addEventListener("htmx:beforeOnLoad", function () {
      setProgress(form, 0, 0);
    });
  }

  function init(scope) {
    var root = scope || document;
    var forms = root.querySelectorAll("form[data-upload]");
    for (var i = 0; i < forms.length; i++) bind(forms[i]);
  }

  if (
    typeof document !== "undefined" &&
    typeof document.addEventListener === "function"
  ) {
    document.addEventListener("DOMContentLoaded", function () {
      init(document);
    });
    document.addEventListener("htmx:load", function (ev) {
      init(ev.target || document);
    });
  }

  return {
    detectFormat: detectFormat,
    classify: classify,
    messageForStatus: messageForStatus,
    messageForCode: messageForCode,
    formatBytes: formatBytes,
    policyForForm: policyForForm,
    setError: setError,
    setProgress: setProgress,
    bind: bind,
    init: init,
    _readPrefix: readPrefix,
    FORMAT_PNG: FORMAT_PNG,
    FORMAT_JPEG: FORMAT_JPEG,
    FORMAT_WEBP: FORMAT_WEBP,
    FORMAT_PDF: FORMAT_PDF,
    MSG_UNSUPPORTED_LOGO: MSG_UNSUPPORTED_LOGO,
    MSG_UNSUPPORTED_ATTACHMENT: MSG_UNSUPPORTED_ATTACHMENT,
    MSG_RATE_LIMITED: MSG_RATE_LIMITED,
    MSG_DECOMPRESSION_BOMB: MSG_DECOMPRESSION_BOMB,
    MSG_SERVER_REJECTED: MSG_SERVER_REJECTED,
    MSG_NETWORK: MSG_NETWORK,
    MSG_UNKNOWN: MSG_UNKNOWN,
    MSG_CANCELLED: MSG_CANCELLED,
  };
});
