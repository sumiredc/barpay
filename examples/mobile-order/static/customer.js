// What the customer pages do in the browser. The server decides what each page shows; this only
// draws the icons, hides the toast, opens and closes the sheets and shows or hides a password.
// - <i data-icon="name"> becomes the SVG of that Lucide icon (ISC license)
// - a .toast goes away after 4 seconds
// - a button with data-sheet="id" opens <dialog class="sheet" id="id">. A sheet has no close
//   button: it closes by a swipe in any direction or a press on the backdrop
// - the button in a .password wrapper shows or hides the password in it
// - <body data-refresh="s"> reloads the page after s seconds, and [data-countdown="s"] counts s
//   seconds down as m:ss and reloads at 0. Neither reloads while a sheet is open, which the reload
//   would close under the finger: the page reloads once the sheet closes.
(function () {
  const ICONS = {
    "arrow-left": '<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>',
    "chevron-right": '<path d="m9 18 6-6-6-6"/>',
    eye: '<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>',
    "eye-off": '<path d="M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.747 10.747 0 0 1-1.444 2.49"/><path d="M14.084 14.158a3 3 0 0 1-4.242-4.242"/><path d="M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143"/><path d="m2 2 20 20"/>',
    info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
    alert: '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
    "circle-check": '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
    "credit-card": '<rect width="20" height="14" x="2" y="5" rx="2"/><path d="M2 10h20"/>',
    utensils: '<path d="M3 2v7c0 1.1.9 2 2 2h4a2 2 0 0 0 2-2V2"/><path d="M7 2v20"/><path d="M21 15V2a5 5 0 0 0-5 5v6c0 1.1.9 2 2 2h3Zm0 0v7"/>',
    settings: '<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>',
    plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
    minus: '<path d="M5 12h14"/>',
    lock: '<rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
    "rotate-cw": '<path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/>',
    gift: '<rect x="3" y="8" width="18" height="4" rx="1"/><path d="M12 8v13"/><path d="M19 12v7a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2v-7"/><path d="M7.5 8a2.5 2.5 0 0 1 0-5A4.8 8 0 0 1 12 8a4.8 8 0 0 1 4.5-5 2.5 2.5 0 0 1 0 5"/>',
    pause: '<rect x="14" y="4" width="4" height="16" rx="1"/><rect x="6" y="4" width="4" height="16" rx="1"/>',
    history: '<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/>',
    qr: '<rect width="5" height="5" x="3" y="3" rx="1"/><rect width="5" height="5" x="16" y="3" rx="1"/><rect width="5" height="5" x="3" y="16" rx="1"/><path d="M21 16h-3a2 2 0 0 0-2 2v3"/><path d="M21 21v.01"/><path d="M12 7v3a2 2 0 0 1-2 2H7"/><path d="M3 12h.01"/><path d="M12 3h.01"/><path d="M12 16v.01"/><path d="M16 12h1"/><path d="M21 12v.01"/><path d="M12 21v-1"/>',
    user: '<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>',
    receipt: '<path d="M4 2v20l2-1 2 1 2-1 2 1 2-1 2 1 2-1 2 1V2l-2 1-2-1-2 1-2-1-2 1-2-1-2 1Z"/><path d="M16 8h-6a2 2 0 1 0 0 4h4a2 2 0 1 1 0 4H8"/><path d="M12 17.5v-11"/>',
  };
  const icon = (name) =>
    `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;

  document.querySelectorAll("i[data-icon]").forEach((el) => { el.outerHTML = icon(el.dataset.icon); });

  document.querySelectorAll(".toast").forEach((t) => setTimeout(() => t.classList.add("gone"), 4000));

  document.querySelectorAll("[data-sheet]").forEach((b) =>
    b.addEventListener("click", () => document.getElementById(b.dataset.sheet).showModal()));
  // A press on the backdrop lands on the dialog itself, outside its content.
  document.querySelectorAll("dialog.sheet").forEach((d) =>
    d.addEventListener("click", (e) => { if (e.target === d) d.close(); }));

  // The sheet follows the finger (or the mouse). Past a distance, or flicked fast enough, it flies
  // off that way and closes; short of it, it springs back. It is not dragged until it has moved
  // 10px, so the buttons in it are pressed as usual.
  document.querySelectorAll("dialog.sheet").forEach((d) => {
    let start = null, dx = 0, dy = 0, dragging = false;
    const reset = () => { d.style.transition = ""; d.style.transform = ""; d.style.opacity = ""; };
    d.addEventListener("pointerdown", (e) => {
      if (e.button !== 0) return;
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

  document.querySelectorAll(".password").forEach((wrap) => {
    const input = wrap.querySelector("input");
    const b = wrap.querySelector("button");
    const render = () => {
      const shown = input.type === "text";
      b.innerHTML = icon(shown ? "eye-off" : "eye");
      b.setAttribute("aria-label", shown ? "Hide password" : "Show password");
    };
    b.addEventListener("click", () => { input.type = input.type === "password" ? "text" : "password"; render(); });
    render();
  });

  let due = false;
  const reload = () => {
    if (document.querySelector("dialog.sheet[open]")) { due = true; return; }
    location.reload();
  };
  document.querySelectorAll("dialog.sheet").forEach((d) =>
    d.addEventListener("close", () => { if (due) location.reload(); }));
  const every = Number(document.body.dataset.refresh);
  if (every > 0) setTimeout(reload, every * 1000);

  // The count starts from the seconds the server computed, not from the phone's clock, which may
  // be off. A count that starts at 0 does not reload: the server already saw the time as over.
  document.querySelectorAll("[data-countdown]").forEach((el) => {
    const seconds = Number(el.dataset.countdown);
    if (!(seconds > 0)) return;
    const end = performance.now() + seconds * 1000;
    const tick = () => {
      const ms = end - performance.now();
      const left = Math.max(0, Math.ceil(ms / 1000));
      el.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
      if (left === 0) { reload(); return; }
      setTimeout(tick, ms % 1000 || 1000);
    };
    tick();
  });
})();
