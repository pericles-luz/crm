// Custom-domain wizard interactions.
//
// SIN-62237 / F29 — replaces the inline onclick / hx-on:click handlers
// previously embedded in wizard_step2.html so the page can ship under a
// strict Content-Security-Policy ("script-src 'self' 'nonce-{N}'", no
// 'unsafe-inline'). The handlers below attach via addEventListener and
// drive the same behaviours via data-* attributes:
//
//   - data-copy-target="<id>"  — clicking the button copies the
//     trimmed textContent of the element with that id to the clipboard.
//   - data-close-target="<sel>"— clicking the button empties the
//     innerHTML of the element matched by that querySelector string.
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
})();
