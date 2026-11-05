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
    header.innerHTML = `<a class="brand" href="admin-sessions.html">${STORE} · スタッフ</a>` +
      (nav === "none" ? "" :
        `<a href="admin-sessions.html">セッション</a>
         <a href="admin-pricing.html">料金</a>
         <a href="admin-alerts.html">アラート</a>
         <a href="admin-notifications.html">通知</a>
         <form data-go="admin-login.html"><button type="submit">ログアウト（${STAFF_ID}）</button></form>`);
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
  document.querySelectorAll("[data-sheet]").forEach((b) =>
    b.addEventListener("click", () => document.getElementById(b.dataset.sheet).showModal()));
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
