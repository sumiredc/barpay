// What the admin pages do in the browser. The server draws the side menu, the dialogs and the
// notices; this only draws the icons, opens and closes the dialogs and reloads the page.
// - <i data-icon="name"> becomes the SVG of that Lucide icon (ISC license)
// - an element with data-sheet="id" opens <dialog class="sheet" id="id"> when pressed, a table
//   row or a chart point also with Enter or Space. A button inside such a row opens only its own
//   dialog, and a dialog opened from another dialog closes that one first. A dialog closes by its
//   [data-close] button, a press on the backdrop, Escape or a swipe in any direction
// - <body data-reload="s"> reloads the page every s seconds, but not while a dialog is open: the
//   reload would throw away what is chosen in it. The page reloads once the dialog closes.
// - <div class="hscroll" data-start="end"> is scrolled to its right end (now) when the page opens
(function () {
  const ICONS = {
    "chevron-right": '<path d="m9 18 6-6-6-6"/>',
    "chevron-left": '<path d="m15 18-6-6 6-6"/>',
    info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
    alert: '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
    "circle-check": '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
    utensils: '<path d="M3 2v7c0 1.1.9 2 2 2h4a2 2 0 0 0 2-2V2"/><path d="M7 2v20"/><path d="M21 15V2a5 5 0 0 0-5 5v6c0 1.1.9 2 2 2h3Zm0 0v7"/>',
    lock: '<rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
    "rotate-cw": '<path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/>',
    history: '<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/>',
    users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
    bell: '<path d="M10.268 21a2 2 0 0 0 3.464 0"/><path d="M3.262 15.326A1 1 0 0 0 4 17h16a1 1 0 0 0 .74-1.673C19.41 13.956 18 12.499 18 8A6 6 0 0 0 6 8c0 4.499-1.411 5.956-2.738 7.326"/>',
    "trending-up": '<path d="M22 7 13.5 15.5 8.5 10.5 2 17"/><path d="M16 7h6v6"/>',
    "log-out": '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>',
    search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  };
  const icon = (name) =>
    `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;

  document.querySelectorAll("i[data-icon]").forEach((el) => { el.outerHTML = icon(el.dataset.icon); });

  const open = (el) => {
    const d = document.getElementById(el.dataset.sheet);
    if (!d) return;
    const from = el.closest("dialog");
    if (from) from.close();
    d.showModal();
  };
  document.querySelectorAll("[data-sheet]").forEach((el) => {
    el.addEventListener("click", (e) => {
      // A button in a row opens its own dialog, not the row's.
      e.stopPropagation();
      open(el);
    });
    if (el.matches("button, a")) return;
    el.addEventListener("keydown", (e) => {
      if (e.target !== el || (e.key !== "Enter" && e.key !== " ")) return;
      e.preventDefault();
      open(el);
    });
  });
  // A press on the backdrop lands on the dialog itself, outside its content.
  document.querySelectorAll("dialog.sheet").forEach((d) =>
    d.addEventListener("click", (e) => { if (e.target === d || e.target.closest("[data-close]")) d.close(); }));

  // The dialog follows the finger (or the mouse). Past a distance, or flicked fast enough, it flies
  // off that way and closes; short of it, it springs back. It is not dragged until it has moved
  // 10px, so the buttons in it are pressed as usual.
  document.querySelectorAll("dialog.sheet").forEach((d) => {
    let start = null, dx = 0, dy = 0, dragging = false;
    const reset = () => { d.style.transition = ""; d.style.transform = ""; d.style.opacity = ""; };
    d.addEventListener("pointerdown", (e) => {
      if (e.button !== 0 || e.target.closest("input, select, textarea")) return;
      start = { x: e.clientX, y: e.clientY, t: Date.now(), id: e.pointerId };
      dx = dy = 0; dragging = false;
    });
    d.addEventListener("pointermove", (e) => {
      if (!start || e.pointerId !== start.id) return;
      dx = e.clientX - start.x; dy = e.clientY - start.y;
      if (!dragging) {
        if (Math.hypot(dx, dy) < 10) return;
        dragging = true;
        d.setPointerCapture(e.pointerId);
        d.style.transition = "none";
      }
      d.style.transform = `translate(${dx}px, ${dy}px) rotate(${dx / 20}deg)`;
    });
    const end = () => {
      if (!start) return;
      const dist = Math.hypot(dx, dy);
      const speed = dist / Math.max(1, Date.now() - start.t);
      start = null;
      if (!dragging) return;
      // The click that follows a drag must not press the button under the finger.
      d.addEventListener("click", (e) => { e.stopPropagation(); e.preventDefault(); }, { capture: true, once: true });
      setTimeout(() => { dragging = false; }, 0);
      if (dist > 120 || (dist > 40 && speed > 0.6)) {
        const k = Math.max(window.innerWidth, window.innerHeight) * 1.5 / dist;
        d.style.transition = "transform 0.3s ease-in, opacity 0.3s";
        d.style.transform = `translate(${dx * k}px, ${dy * k}px) rotate(${(dx * k) / 20}deg)`;
        d.style.opacity = "0";
        setTimeout(() => { d.close(); reset(); }, 300);
      } else {
        d.style.transition = "transform 0.25s cubic-bezier(.2,.8,.2,1.2)";
        d.style.transform = "";
      }
    };
    d.addEventListener("pointerup", end);
    d.addEventListener("pointercancel", end);
  });

  document.querySelectorAll('.hscroll[data-start="end"]').forEach((el) => { el.scrollLeft = el.scrollWidth; });

  let due = false;
  const reload = () => {
    if (document.querySelector("dialog.sheet[open]")) { due = true; return; }
    location.reload();
  };
  document.querySelectorAll("dialog.sheet").forEach((d) =>
    d.addEventListener("close", () => { if (due) location.reload(); }));
  const every = Number(document.body.dataset.reload);
  if (every > 0) setInterval(reload, every * 1000);
})();
