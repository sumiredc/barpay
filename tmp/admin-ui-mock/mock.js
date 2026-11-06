// モックの共通の動き。
// - ヘッダーを描く。客の画面は店名だけにし、来店中（伝票・メニューなど）は下のタブバーを出す。nav が "none" のとき
//   （入口・ログイン・会員登録・エラー）はどちらも出さず、画面が自分で戻るボタンを持つ
// - 状態は URL の ?v= で選ぶ。各状態へのリンクは index.html にある
// - ?toast= で、移った先の画面の上に短い通知を出す（アプリの flash にあたる）
// - ?sheet= で、その画面のシート（<dialog class="sheet" id="…">）を開いた状態で出す
// - フォームは送信せず、data-go があればその画面へ移る
// - <i data-icon="名前"> を、Lucide（ISC ライセンス）のアイコンの SVG に置き換える
(function () {
  const STORE = "The Copper Fox";
  const STAFF_ID = "staff-01";
  const ICONS = {
    "arrow-left": '<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>',
    "chevron-right": '<path d="m9 18 6-6-6-6"/>',
    "chevron-left": '<path d="m15 18-6-6 6-6"/>',
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
    users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
    bell: '<path d="M10.268 21a2 2 0 0 0 3.464 0"/><path d="M3.262 15.326A1 1 0 0 0 4 17h16a1 1 0 0 0 .74-1.673C19.41 13.956 18 12.499 18 8A6 6 0 0 0 6 8c0 4.499-1.411 5.956-2.738 7.326"/>',
    "trending-up": '<path d="M22 7 13.5 15.5 8.5 10.5 2 17"/><path d="M16 7h6v6"/>',
    "log-out": '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>',
    search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
    receipt: '<path d="M4 2v20l2-1 2 1 2-1 2 1 2-1 2 1 2-1 2 1V2l-2 1-2-1-2 1-2-1-2 1-2-1-2 1Z"/><path d="M16 8h-6a2 2 0 1 0 0 4h4a2 2 0 1 1 0 4H8"/><path d="M12 17.5v-11"/>',
  };
  const TOASTS = {
    welcome: "ようこそ、Taro さん。アカウントができました",
    checkin: "チェックインしました",
    ordered: "ご注文を承りました",
    "already-ordered": "この注文は受付済みです",
    upgraded: "会員になりました。今の来店はそのまま続きます",
  };
  const icon = (name) =>
    `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;

  const body = document.body;
  const variants = JSON.parse(body.dataset.variants || "[]");
  const params = new URLSearchParams(location.search);
  const current = variants.find((v) => v.k === params.get("v")) || variants[0];

  // ヘッダー
  const header = document.createElement("header");
  const nav = current ? current.nav : body.dataset.nav;
  if (body.dataset.app === "index" || (body.dataset.app === "customer" && nav === "none")) {
    // ヘッダーを出さない
  } else if (body.dataset.app === "admin") {
    // 左のサイドメニュー。来店・アラート・料金・ログを上に並べ、来店には精算待ち、アラートには未対応の件数を出す。
    // 通知の記録とログアウトは下に控えめに置く。ログインの画面はメニューを出さず、店名だけにする
    const page = location.pathname.split("/").pop();
    // 来店には精算待ちの件数を出す。精算待ちが無い状態では出さない
    const waiting = page === "admin-sessions.html" && current && current.k === "empty" ? 0 : 3;
    const unhandled = page === "admin-alerts.html" && current && current.k === "empty" ? 0 : 1;
    // 料金の設定は「料金」の中のタブなので、設定の画面でも「料金」を今の画面にする
    const here = page === "admin-settings.html" ? "admin-pricing.html" : page;
    const link = (href, ic, label, extra = "") =>
      `<a href="${href}"${here === href ? ' aria-current="page"' : ""}>${icon(ic)}<span>${label}</span>${extra}</a>`;
    if (nav === "none") {
      header.innerHTML = `<span class="brand">${STORE} · スタッフ</span>`;
    } else {
      const side = document.createElement("aside");
      side.className = "sidenav";
      side.innerHTML =
        `<a class="brand" href="admin-sessions.html"><img src="logo.svg" alt=""><span>${STORE}</span></a>
         <nav aria-label="スタッフの画面">
           ${link("admin-sessions.html", "users", "来店", waiting ? `<span class="badge" aria-label="精算待ち ${waiting} 件">${waiting}</span>` : "")}
           ${link("admin-orders.html", "utensils", "注文", `<span class="badge" aria-label="未提供 3 件">3</span>`)}
           ${link("admin-alerts.html", "bell", "アラート", unhandled ? `<span class="badge" aria-label="未対応 ${unhandled} 件">${unhandled}</span>` : "")}
           ${link("admin-pricing.html", "trending-up", "料金")}
           ${link("admin-notifications.html", "history", "ログ")}
         </nav>
         <div class="sub">
           <button type="button" data-sheet="gate-manual">${icon("lock")}<span>解錠</span></button>
           <form data-go="admin-login.html"><button type="submit">${icon("log-out")}<span>ログアウト<small>${STAFF_ID}</small></span></button></form>
         </div>`;
      body.prepend(side);
      body.classList.add("has-sidenav");
      // 退店ゲートの解錠の確認は、どの画面からも開ける
      const gate = document.createElement("dialog");
      gate.className = "sheet";
      gate.id = "gate-manual";
      gate.innerHTML = `<form data-go="admin-sessions.html?v=gate-sent">
        <h2>退店ゲートを解錠する</h2>
        <p class="muted">誰のために解錠しますか</p>
        <label class="choice"><input type="radio" name="for" required> Yui Tanaka（21:59 に PayPal で精算）</label>
        <label class="choice"><input type="radio" name="for"> 特定のお客様のためではない</label>
        <p class="warn">解錠しているあいだは、支払っていないお客様も通れます。解錠したスタッフは記録に残ります。</p>
        <div class="dialog-actions"><button class="textbutton muted" type="button" data-close>やめる</button><button class="button" type="submit">解錠する</button></div>
      </form>`;
      body.append(gate);
    }
  } else {
    // 客の画面：上は小さなロゴと店名だけ。押せるものは置かず、固定もしないのでスクロールで消える。
    // 行き来は下のタブバーで行う。来店履歴・PayPal・ログアウトはアカウントの画面にある。入口は大きなロゴを出すので、ここでは出さない
    header.className = "brandmark";
    if (location.pathname.split("/").pop() !== "landing.html") {
      header.innerHTML = `<img src="logo.svg" alt=""><span>${STORE}</span>`;
    }
    if (nav === "member" || nav === "guest") {
      const page = location.pathname.split("/").pop();
      const tab = { "menu.html": "menu", "panel.html": "panel", "exit.html": "panel",
        "account.html": "account", "receipts.html": "account", "receipt.html": "account", "paypal.html": "account" }[page];
      const tabs = [["menu", "utensils", "メニュー"], ["panel", "receipt", "伝票"],
        ["account", "user", "アカウント"]];
      const bar = document.createElement("nav");
      bar.className = "tabbar";
      bar.setAttribute("aria-label", "画面");
      bar.innerHTML = tabs.map(([k, i, l]) =>
        `<a href="${k}.html${k === "account" && nav === "guest" ? "?v=guest" : ""}"${k === tab ? ' aria-current="page"' : ""}>${icon(i)}<span>${l}</span></a>`).join("");
      body.append(bar);
      body.classList.add("has-tabbar");
    }
  }
  if (header.innerHTML) body.prepend(header);

  // ?from=guest：来店中のゲストがアカウントから会員登録・ログインに来たとき。
  // 戻る先と終わったあとの行き先をアカウントにし、今の来店を引き継ぐことを示す要素（data-from="guest"）を出す
  const fromGuest = params.get("from") === "guest";
  document.querySelectorAll("[data-from]").forEach((el) => { if (!fromGuest) el.remove(); });
  if (fromGuest) {
    document.querySelectorAll("a.back").forEach((a) => { a.href = "account.html?v=guest"; });
    document.querySelectorAll("form.form[data-go]").forEach((f) => { f.dataset.go = "account.html?v=unlinked&toast=upgraded"; });
    document.querySelectorAll(".switch a").forEach((a) => { a.href += (a.href.includes("?") ? "&" : "?") + "from=guest"; });
  }

  // 状態に合う要素だけを残す（data-v は空白区切りで複数の状態を書ける）。消すので、状態ごとに同じ id を使える
  document.querySelectorAll("[data-v]").forEach((el) => {
    if (!current || !el.dataset.v.split(/\s+/).includes(current.k)) el.remove();
  });

  // 下に固定したボタンがあれば、本文がその下に隠れないように余白を足す
  if ([...document.querySelectorAll(".cartbar")].some((b) => !b.hidden)) body.classList.add("has-cart");

  // アイコン
  document.querySelectorAll("i[data-icon]").forEach((el) => { el.outerHTML = icon(el.dataset.icon); });

  // 通知（トースト）。数秒で消える
  const toast = TOASTS[params.get("toast")];
  if (toast) {
    const t = document.createElement("div");
    t.className = "toast";
    t.setAttribute("role", "status");
    t.innerHTML = `${icon("circle-check")}<span>${toast}</span>`;
    body.append(t);
    setTimeout(() => t.classList.add("gone"), 4000);
  }

  // シート：data-sheet="id" のボタンで開き、シートの中の data-close か背景で閉じる
  // 行（tr）にも付けられる。行の中のボタンを押したときは、行の分は開かない。
  // シートの中から別のシートを開くときは、今のシートを閉じてから開く
  document.querySelectorAll("[data-sheet]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      const from = b.closest("dialog");
      if (from) from.close();
      document.getElementById(b.dataset.sheet).showModal();
    }));
  // 選んだ方法（name="how" のラジオ）に合う部分だけを出す
  document.querySelectorAll("form").forEach((f) => {
    const hows = f.querySelectorAll('input[name="how"]');
    if (!hows.length) return;
    const show = () => {
      const v = (f.querySelector('input[name="how"]:checked') || {}).value;
      f.querySelectorAll("[data-how]").forEach((el) => { el.hidden = el.dataset.how !== v; });
    };
    hows.forEach((h) => h.addEventListener("change", show));
    show();
  });
  document.querySelectorAll("dialog.sheet").forEach((d) =>
    d.addEventListener("click", (e) => { if (e.target === d || e.target.closest("[data-close]")) d.close(); }));

  // シートはカードにして、どの方向でもスワイプすれば消える。指（マウス）について動き、
  // 一定の距離を超えるか素早く払うと、その方向へ飛んで閉じる。届かなければ元に戻る。
  // 動かし始めて少し進むまではドラッグと見なさないので、中のボタンは普通に押せる
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
      // ドラッグの直後に出る click で、中のボタンが押されないようにする
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
  const opened = params.get("sheet") && document.getElementById(params.get("sheet"));
  if (opened && opened.showModal) opened.showModal();

  // パスワードの表示を切り替える
  document.querySelectorAll(".password").forEach((wrap) => {
    const input = wrap.querySelector("input");
    const b = wrap.querySelector("button");
    const render = () => {
      const shown = input.type === "text";
      b.innerHTML = icon(shown ? "eye-off" : "eye");
      b.setAttribute("aria-label", shown ? "パスワードを隠す" : "パスワードを表示");
    };
    b.addEventListener("click", () => { input.type = input.type === "password" ? "text" : "password"; render(); });
    render();
  });


  // 設定の見込み：入力した範囲・来店の上限・間隔・注文後の無料時間から、ドリンク 1〜3 杯それぞれの支払いを
  // その場で計算し直す。ドリンクは 0・30・60 分に頼むとし、2 杯目からは無料時間のあいだ加算しない
  const preview = document.getElementById("preview");
  if (preview) {
    const num = (id) => { const el = document.getElementById(id); const v = parseFloat(el && el.value); return Number.isFinite(v) ? v : NaN; };
    const usd = (v) => `$${v.toFixed(2)}`;
    const drink = parseFloat(preview.dataset.drink);
    const ORDER_AT = [0, 30, 60];
    const render = () => {
      const floor = num("floor"), ceiling = num("ceiling"), cap = num("cap"), block = num("block"), pause = num("pause");
      const ok = [floor, ceiling, cap, block, pause].every(Number.isFinite) && block >= 1 && ceiling >= floor;
      preview.classList.toggle("stale", !ok);
      if (!ok) return;
      const now = Math.min(Math.max(parseFloat(preview.dataset.rate), floor), ceiling);
      const basis = (preview.querySelector('input[name="basis"]:checked') || {}).value || "now";
      const rate = { floor, now, ceiling }[basis];
      // 滞在 m 分までの支払い（チャージ料と、それまでに頼んだドリンク）
      const pay = (m, cups) => {
        const orders = ORDER_AT.slice(0, cups);
        const paused = (end) => orders.slice(1).some((t) => end >= t && end < t + pause);
        let charge = 0;
        for (let e = block; e <= m; e += block) if (!paused(e)) charge += rate * block / 60;
        charge = Math.round(charge * 100) / 100;
        if (cap > 0) charge = Math.min(charge, cap);
        const d = orders.filter((t) => t <= m).length * drink;
        return { charge, drink: d, total: charge + d };
      };
      preview.querySelectorAll('[data-p="floor"]').forEach((el) => { el.textContent = usd(floor); });
      preview.querySelectorAll('[data-p="ceiling"]').forEach((el) => { el.textContent = usd(ceiling); });
      preview.querySelectorAll('[data-p="now"]').forEach((el) => { el.textContent = usd(now); });
      preview.querySelectorAll('[data-p="drink"]').forEach((el) => { el.textContent = usd(drink); });
      const view = (preview.querySelector('input[name="view"]:checked') || {}).value || "total";
      const val = (r) => (view === "charge" ? r.charge : r.total);
      const head = pay(120, 2);
      preview.querySelector('[data-p="headline"]').textContent = usd(val(head));
      preview.querySelector('[data-p="headsub"]').hidden = view === "charge";
      preview.querySelector('[data-p="headcharge"]').textContent = usd(head.charge);
      preview.querySelector('[data-p="headdrink"]').textContent = usd(head.drink);
      // グラフ：横は滞在 0〜4 時間、縦は支払い。加算とドリンクで階段になる
      const svg = preview.querySelector("svg.preview-chart");
      const W = Math.max(320, Math.round(svg.getBoundingClientRect().width) || 640), H = 240, L = 52, R = 84, T = 20, B = 32, maxM = 240;
      svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
      const top = Math.max(...[1, 2, 3].map((c) => val(pay(maxM, c))), 1);
      const yMax = Math.ceil(top / 10) * 10 + 5;
      const x = (m) => L + (m / maxM) * (W - L - R);
      const y = (v) => T + (1 - v / yMax) * (H - T - B);
      let g = "";
      for (let v = 0; v <= yMax; v += 10) g += `<line class="grid" x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}"/><text class="tick" x="${L - 8}" y="${y(v) + 4}" text-anchor="end">$${v}</text>`;
      for (let h = 0; h <= 4; h++) g += `<text class="tick" x="${x(h * 60)}" y="${H - 10}" text-anchor="${h === 0 ? "start" : "middle"}">${h} 時間</text>`;
      // 2 杯目・3 杯目のあとの無料時間を帯で示す（その杯数を頼んだ線にだけ効く）
      if (pause > 0) ORDER_AT.slice(1).forEach((t, i) => {
        const x1 = x(t), x2 = x(Math.min(t + pause, maxM));
        g += `<rect class="free" x="${x1}" y="${T}" width="${Math.max(1, x2 - x1)}" height="${H - T - B}"/>`;
        g += `<text class="tick free-label" x="${x1 + 3}" y="${T + 12}">${i + 2} 杯目</text>`;
      });
      g += `<line class="mark" x1="${x(120)}" x2="${x(120)}" y1="${T}" y2="${H - B}"/>`;
      // 右の数字は各線の右端（4 時間）の額。線の終わりに合わせ、重なるときは 14px ずつ離す
      const labelY = {};
      [1, 2, 3].map((c) => [c, y(val(pay(maxM, c)))]).sort((p, q) => p[1] - q[1])
        .forEach(([c, yy], i, arr) => { labelY[c] = i === 0 ? yy : Math.max(yy, labelY[arr[i - 1][0]] + 14); });
      [1, 2, 3].forEach((cups) => {
        const pts = new Set([0, maxM]);
        for (let e = block; e <= maxM; e += block) pts.add(e);
        ORDER_AT.slice(0, cups).forEach((t) => pts.add(t));
        const ms = [...pts].sort((p, q) => p - q);
        let d = `M${x(0)},${y(val(pay(0, cups)))}`;
        ms.slice(1).forEach((m) => { d += `H${x(m)}V${y(val(pay(m, cups)))}`; });
        const end = val(pay(maxM, cups));
        g += `<path class="line d${cups}" d="${d}"/><text class="tick end d${cups}" x="${W - R + 6}" y="${labelY[cups] + 4}">${cups} 杯 ${usd(end)}</text>`;
        g += `<circle class="dot d${cups}" cx="${x(120)}" cy="${y(val(pay(120, cups)))}" r="4"/>`;
      });
      svg.innerHTML = g;
      const freeLegend = preview.querySelector('[data-p="freelegend"]');
      if (freeLegend) freeLegend.hidden = !(pause > 0);
    };
    window.addEventListener("resize", render);
    preview.querySelectorAll('input[name="basis"], input[name="view"]').forEach((el) => el.addEventListener("change", render));
    ["floor", "ceiling", "cap", "block", "pause"].forEach((id) =>
      document.querySelectorAll(`#${id}`).forEach((el) => el.addEventListener("input", render)));
    render();
  }

  // 料金の方針：つまみの位置に応じて、AI に伝わる目標を、混み具合ごとの料金の向きで見せる
  const goal = document.getElementById("goal");
  const effect = document.getElementById("goal-effect");
  if (goal && effect) {
    const words = (v) => v <= 20 ? ["下限近くまで下げる", "据え置く"]
      : v <= 40 ? ["下げる", "少し上げる"]
      : v <= 60 ? ["少し下げる", "上げる"]
      : v <= 80 ? ["据え置く", "上げる"]
      : ["据え置く", "上限近くまで上げる"];
    const show = () => {
      const [low, high] = words(Number(goal.value));
      effect.querySelector('[data-g="low"]').textContent = low;
      effect.querySelector('[data-g="high"]').textContent = high;
    };
    goal.addEventListener("input", show);
    show();
  }

  // 横にスクロールするグラフは、右端（今）から見せる
  document.querySelectorAll('.hscroll[data-start="end"]').forEach((el) => { el.scrollLeft = el.scrollWidth; });

  // フォームは送らない
  document.addEventListener("submit", (e) => {
    e.preventDefault();
    const go = e.target.dataset.go;
    if (go) location.href = go;
  });

  // 残り時間の表示。data-countdown の秒数から 1 秒ずつ減らす
  document.querySelectorAll("[data-countdown]").forEach((el) => {
    let left = Number(el.dataset.countdown);
    const show = () => { el.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`; };
    show();
    setInterval(() => { if (left > 0) { left--; show(); } }, 1000);
  });

  // 退店 QR の代わりの絵。本物と同じく、一定の間隔で変わる
  document.querySelectorAll("svg.mock-qr").forEach((svg) => {
    const n = 29;
    const draw = () => {
      let cells = "";
      for (let y = 0; y < n; y++) {
        for (let x = 0; x < n; x++) {
          const finder = (a, b) => a >= 0 && a < 7 && b >= 0 && b < 7 &&
            (a === 0 || a === 6 || b === 0 || b === 6 || (a >= 2 && a <= 4 && b >= 2 && b <= 4));
          const inFinder = (x < 8 && y < 8) || (x >= n - 8 && y < 8) || (x < 8 && y >= n - 8);
          const on = inFinder
            ? finder(x, y) || finder(x - (n - 7), y) || finder(x, y - (n - 7))
            : Math.random() < 0.45;
          if (on) cells += `<rect x="${x}" y="${y}" width="1" height="1"/>`;
        }
      }
      svg.innerHTML = `<rect x="-2" y="-2" width="${n + 4}" height="${n + 4}" fill="#fff"/><g fill="#000">${cells}</g>`;
    };
    svg.setAttribute("viewBox", `-2 -2 ${n + 4} ${n + 4}`);
    draw();
    setInterval(draw, 5000);
  });
})();
