// Custom-domain wizard interactions.
//
// SIN-62237 / F29 — replaces the inline onclick / hx-on:* handlers
// previously embedded in the customdomain templates so every page can
// ship under the strict Content-Security-Policy emitted by the csp
// middleware ("script-src 'self' 'nonce-{N}'", no 'unsafe-inline', no
// 'unsafe-eval'). htmx 2.x evaluates `hx-on:*` attributes via
// `new Function(...)` which CSP's `'unsafe-eval'` gate blocks; this
// module delegates the same behaviours via data-* attributes instead:
//
//   - data-copy-target="<id>"            click → copy the trimmed
//     textContent of the element with that id to the clipboard.
//   - data-close-target="<sel>"          click → empty innerHTML of
//     the element matched by querySelector(sel).
//   - data-close-on-success="<sel>"      htmx:afterRequest with a
//     successful response → empty innerHTML of querySelector(sel).
//     Use on the <form>/element that issues the htmx request.
//
// Event delegation on document keeps the wiring resilient when HTMX
// swaps wizard fragments in and out.

(function () {
  'use strict';

  function findClosest(target, attr) {
    var node = target;
    while (node && node !== document) {
      if (node.nodeType === 1 && node.hasAttribute && node.hasAttribute(attr)) {
        return node;
      }
      node = node.parentNode;
    }
    return null;
  }

  document.addEventListener('click', function (event) {
    var copyBtn = findClosest(event.target, 'data-copy-target');
    if (copyBtn) {
      event.preventDefault();
      var id = copyBtn.getAttribute('data-copy-target');
      var src = id ? document.getElementById(id) : null;
      if (src && navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
        navigator.clipboard.writeText(src.textContent.trim());
      }
      return;
    }
    var closeBtn = findClosest(event.target, 'data-close-target');
    if (closeBtn) {
      event.preventDefault();
      var selector = closeBtn.getAttribute('data-close-target');
      if (selector) {
        var dst = document.querySelector(selector);
        if (dst) {
          dst.innerHTML = '';
        }
      }
    }
  });

  // htmx:afterRequest fires once on the element that issued the request.
  // We close the linked container only when the response was successful
  // (2xx). The htmx CustomEvent carries a `successful` flag on
  // event.detail; falling back to detail.xhr.status protects against an
  // older htmx that doesn't populate that helper.
  document.addEventListener('htmx:afterRequest', function (event) {
    var el = event.target;
    if (!el || !el.hasAttribute || !el.hasAttribute('data-close-on-success')) {
      return;
    }
    var detail = event.detail || {};
    var ok = detail.successful === true;
    if (!ok && detail.xhr && typeof detail.xhr.status === 'number') {
      ok = detail.xhr.status >= 200 && detail.xhr.status < 300;
    }
    if (!ok) {
      return;
    }
    var selector = el.getAttribute('data-close-on-success');
    if (!selector) {
      return;
    }
    var dst = document.querySelector(selector);
    if (dst) {
      dst.innerHTML = '';
    }
  });
})();
