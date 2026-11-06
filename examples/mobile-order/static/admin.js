// What the admin pages do in the browser. The server draws the side menu, the dialogs and the
// notices; this only draws the icons, opens and closes the dialogs and reloads the page.
// - <i data-icon="name"> becomes the SVG of that Lucide icon (ISC license)
// - an element with data-sheet="id" opens <dialog class="sheet" id="id"> when pressed, a table
//   row or a chart point also with Enter or Space. A button inside such a row opens only its own
//   dialog, or none when it has none, and a dialog opened from another dialog closes that one
//   first. A dialog closes by its [data-close] button, a press on the backdrop, Escape or a swipe
//   in any direction
// - a form in a dialog with radios named "how" shows only the [data-how] parts of the chosen one
// - <body data-reload="s"> reloads the page every s seconds, but not while a dialog is open: the
//   reload would throw away what is chosen in it. The page reloads once the dialog closes.
// - <div class="hscroll" data-start="end"> is scrolled to its right end (now) when the page opens
// - #goal-effect says, under the pricing goal's slider (#goal), which way the goal asks the AI to
//   move the rate when the room is quiet and when it is busy. It is a rough guide in five steps:
//   what the AI is given is the goal's number and what its two ends mean
// - <section id="preview"> works out what one customer pays with the settings in the form, and
//   draws it again on every change, without saving and without asking the server. It follows the
//   cover charge rules of docs/PRD.md (4. Pricing model); see the comment above coverCharge.
//   While the form cannot be saved, the card is faded and keeps what it drew last
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
      // A button in a row opens its own dialog, not the row's; one with no dialog of its own, such
      // as a form's, opens none.
      if (e.target.closest("button, a, input, label, form") !== el.closest("button, a, input, label, form")) return;
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

  const goal = document.getElementById("goal");
  const effect = document.getElementById("goal-effect");
  if (goal && effect) {
    const words = (v) => v <= 20 ? ["Down to near the floor", "Keep"]
      : v <= 40 ? ["Lower", "A little higher"]
      : v <= 60 ? ["A little lower", "Higher"]
      : v <= 80 ? ["Keep", "Higher"]
      : ["Keep", "Up to near the ceiling"];
    const show = () => {
      const [quiet, busy] = words(Number(goal.value));
      effect.querySelector('[data-g="low"]').textContent = quiet;
      effect.querySelector('[data-g="high"]').textContent = busy;
    };
    goal.addEventListener("input", show);
    show();
  }

  const preview = document.getElementById("preview");
  if (preview) drawPreview(preview);

  // drawPreview keeps the card "What a customer pays" up to date with the form. Amounts are in
  // cents, as the app's amounts are in USD.
  function drawPreview(preview) {
    // The drink is ordered on arrival, 30 and 60 minutes later; the line of n drinks orders the
    // first n. The headline is a stay of 2 hours with 2 drinks.
    const ORDER_AT = [0, 30, 60], MAX_MINUTES = 240, HEAD_MINUTES = 120, HEAD_DRINKS = 2;
    // BarPay's rate step and largest amount, in cents (docs/api/openapi.yaml: Settings, Money).
    const RATE_STEP = 5, MAX_AMOUNT = 100000000;
    const drink = Number(preview.dataset.drink);
    const published = preview.dataset.rate === undefined ? null : Number(preview.dataset.rate);
    const usd = (cents) => {
      const s = String(Math.floor(cents / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
      return `$${s}.${String(cents % 100).padStart(2, "0")}`;
    };
    // An amount as staff type it, such as 12.50, in cents (the server reads it the same way).
    const cents = (s) => {
      const m = /^\s*\$?(\d+)(?:\.(\d{0,2}))?\s*$/.exec(s);
      return m ? Number(m[1]) * 100 + Number((m[2] || "").padEnd(2, "0")) : NaN;
    };
    const whole = (s) => (/^\s*\d+\s*$/.test(s) ? Number(s) : NaN);
    const field = (id) => (document.getElementById(id) || {}).value || "";
    // s when BarPay would take it, otherwise null. It also keeps the loops over the blocks finite:
    // the block is a whole number of minutes from 1.
    const valid = (s) => {
      const ok = Number.isInteger(s.floor) && Number.isInteger(s.ceiling) && Number.isInteger(s.cap) &&
        Number.isInteger(s.block) && Number.isInteger(s.pause) &&
        s.floor > 0 && s.floor % RATE_STEP === 0 && s.ceiling % RATE_STEP === 0 && s.ceiling > s.floor &&
        s.ceiling <= MAX_AMOUNT && s.cap >= 0 && s.cap <= MAX_AMOUNT && s.block >= 1 && s.block <= 240 && s.pause >= 0 && s.pause <= 240;
      return ok ? s : null;
    };
    // The settings the form holds, or null while BarPay would refuse them.
    const fromForm = () => valid({ floor: cents(field("floor")), ceiling: cents(field("ceiling")), cap: cents(field("cap")),
      block: whole(field("block")), pause: whole(field("pause")) });
    // The saved settings from the server, or null when there are none or they are not usable, and
    // then the card is not drawn from them.
    const d = preview.dataset;
    const saved = d.floor === undefined ? null
      : valid({ floor: Number(d.floor), ceiling: Number(d.ceiling), cap: Number(d.cap), block: Number(d.block), pause: Number(d.pause) });

    // coverCharge is the cover charge of a stay of `minutes` at `rate` cents per hour, for a
    // customer who ordered at each of `orders` (minutes after check-in):
    // - every block of s.block minutes is charged when it ends, at rate × block length rounded
    //   down to the cent; a block that has not ended is not charged
    // - the first order does not count. Each later order pauses the charge for s.pause minutes
    //   (0 for none) from that order: a block ending in a pause costs 0. A pause includes the
    //   instant of its order, so a block ending exactly at the order costs 0, and ends just
    //   before s.pause minutes have passed, so a block ending exactly then is charged
    // - the charge stops at s.cap (0 for none): the block that reaches it is charged only up to it
    const coverCharge = (minutes, orders, rate, s) => {
      const perBlock = Math.floor((rate * s.block) / 60);
      const paused = (end) => s.pause > 0 && orders.slice(1).some((t) => t <= end && end < t + s.pause);
      let charge = 0;
      for (let end = s.block; end <= minutes; end += s.block) {
        if (paused(end)) continue;
        charge += perBlock;
        if (s.cap > 0 && charge >= s.cap) return s.cap;
      }
      return charge;
    };
    const pay = (minutes, drinks, rate, s) => {
      const orders = ORDER_AT.slice(0, drinks);
      const charge = coverCharge(minutes, orders, rate, s);
      const drinksTotal = orders.filter((t) => t <= minutes).length * drink;
      return { charge, drinks: drinksTotal, total: charge + drinksTotal };
    };
    // The y axis goes up in steps of 1, 2 or 5 × 10^n dollars, about five of them.
    const niceStep = (x) => {
      const p = Math.pow(10, Math.floor(Math.log10(Math.max(x, 100))));
      return [1, 2, 5, 10].map((k) => k * p).find((v) => v >= x);
    };

    let last = null;
    const render = () => {
      const s = fromForm() || (last ? null : saved);
      preview.classList.toggle("stale", !fromForm());
      if (s) last = s;
      if (!last) return;
      draw(last);
    };
    const draw = (s) => {
      // "Now" is the published rate moved into the range being entered, as saving moves it there.
      const now = published === null ? null : Math.min(Math.max(published, s.floor), s.ceiling);
      const basis = (preview.querySelector('input[name="basis"]:checked') || {}).value;
      const rate = basis === "now" && now !== null ? now : basis === "ceiling" ? s.ceiling : s.floor;
      const set = (key, text) => preview.querySelectorAll(`[data-p="${key}"]`).forEach((el) => { el.textContent = text; });
      set("floor", usd(s.floor));
      set("ceiling", usd(s.ceiling));
      if (now !== null) set("now", usd(now));
      const view = (preview.querySelector('input[name="view"]:checked') || {}).value;
      const val = (r) => (view === "charge" ? r.charge : r.total);
      const head = pay(HEAD_MINUTES, HEAD_DRINKS, rate, s);
      set("headline", usd(val(head)));
      set("headcharge", usd(head.charge));
      set("headdrink", usd(head.drinks));
      preview.querySelector('[data-p="headsub"]').hidden = view === "charge";
      preview.querySelector('[data-p="freelegend"]').hidden = !(s.pause > 0);

      // The chart: the stay from 0 to 4 hours across, the amount up. Blocks and drinks make steps.
      const svg = preview.querySelector("svg.preview-chart");
      const W = Math.max(320, Math.round(svg.getBoundingClientRect().width) || 640), H = 240, L = 52, R = 104, T = 20, B = 32;
      svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
      const ends = [1, 2, 3].map((n) => val(pay(MAX_MINUTES, n, rate, s)));
      const top = Math.max(...ends, 100);
      const step = niceStep(top / 5);
      const yMax = Math.ceil(top / step) * step + step / 2;
      const x = (m) => L + (m / MAX_MINUTES) * (W - L - R);
      const y = (v) => T + (1 - v / yMax) * (H - T - B);
      let g = "";
      for (let v = 0; v <= yMax; v += step) {
        g += `<line class="grid" x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}"/>`;
        g += `<text class="tick" x="${L - 8}" y="${y(v) + 4}" text-anchor="end">${usd(v).replace(/\.00$/, "")}</text>`;
      }
      for (let h = 0; h <= MAX_MINUTES / 60; h++) {
        g += `<text class="tick" x="${x(h * 60)}" y="${H - 10}" text-anchor="${h === 0 ? "start" : "middle"}">${h} h</text>`;
      }
      // The pause after the second and the third drink, which only the lines with that drink have.
      if (s.pause > 0) ORDER_AT.slice(1).forEach((t, i) => {
        const x1 = x(t), x2 = x(Math.min(t + s.pause, MAX_MINUTES));
        g += `<rect class="free" x="${x1}" y="${T}" width="${Math.max(1, x2 - x1)}" height="${H - T - B}"/>`;
        g += `<text class="tick free-label" x="${x1 + 3}" y="${T + 12}">Drink ${i + 2}</text>`;
      });
      g += `<line class="mark" x1="${x(HEAD_MINUTES)}" x2="${x(HEAD_MINUTES)}" y1="${T}" y2="${H - B}"/>`;
      // The amount at 4 hours beside the end of each line, 14px apart where they would overlap.
      const labelY = {};
      [1, 2, 3].map((n) => [n, y(ends[n - 1])]).sort((p, q) => p[1] - q[1])
        .forEach(([n, yy], i, arr) => { labelY[n] = i === 0 ? yy : Math.max(yy, labelY[arr[i - 1][0]] + 14); });
      [1, 2, 3].forEach((n) => {
        const at = new Set([0, MAX_MINUTES, ...ORDER_AT.slice(0, n)]);
        for (let e = s.block; e <= MAX_MINUTES; e += s.block) at.add(e);
        const ms = [...at].sort((p, q) => p - q);
        let path = `M${x(0)},${y(val(pay(0, n, rate, s)))}`;
        ms.slice(1).forEach((m) => { path += `H${x(m)}V${y(val(pay(m, n, rate, s)))}`; });
        g += `<path class="line d${n}" d="${path}"/>`;
        g += `<text class="tick end d${n}" x="${W - R + 6}" y="${labelY[n] + 4}">${n} drink${n > 1 ? "s" : ""} ${usd(ends[n - 1])}</text>`;
        g += `<circle class="dot d${n}" cx="${x(HEAD_MINUTES)}" cy="${y(val(pay(HEAD_MINUTES, n, rate, s)))}" r="4"/>`;
      });
      svg.innerHTML = g;
    };
    window.addEventListener("resize", () => { if (last) draw(last); });
    preview.querySelectorAll('input[name="basis"], input[name="view"]').forEach((el) => el.addEventListener("change", () => { if (last) draw(last); }));
    ["floor", "ceiling", "cap", "block", "pause"].forEach((id) => {
      const el = document.getElementById(id);
      if (el) el.addEventListener("input", render);
    });
    render();
  }

  // A form with ways to choose (radios named "how") shows only the parts of the chosen way
  // ([data-how]). The inputs of the other parts are disabled, so they are neither required nor
  // sent. Typing another amount chooses it.
  document.querySelectorAll("dialog.sheet form").forEach((f) => {
    const hows = f.querySelectorAll('input[name="how"]');
    if (!hows.length) return;
    const show = () => {
      const v = (f.querySelector('input[name="how"]:checked') || {}).value;
      f.querySelectorAll("[data-how]").forEach((el) => {
        el.hidden = el.dataset.how !== v;
        el.querySelectorAll("input").forEach((i) => { i.disabled = el.hidden; });
      });
    };
    hows.forEach((h) => h.addEventListener("change", show));
    show();
    f.querySelectorAll("input[data-other]").forEach((t) => {
      const radio = t.closest("label").querySelector('input[type="radio"]');
      const sync = () => { t.required = radio.checked; };
      t.addEventListener("input", () => { radio.checked = true; sync(); });
      f.querySelectorAll('input[name="' + radio.name + '"]').forEach((r) => r.addEventListener("change", sync));
    });
  });

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
