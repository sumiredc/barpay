// The cart of the menu page. Pressing an item opens its sheet, where the customer picks the
// options and the quantity and puts it in the cart. The same item with other options is another
// line; the same item with the same options adds to its line. Pressing a line of the order sheet
// opens that line in the item sheet, and the order sheet comes back after it.
// The cart lives in the page only. The order sends the item IDs, the picked options and the
// quantities, and the server prices them from its menu.
(function () {
  const menu = document.querySelector(".menu");
  if (!menu) return;
  // { id: { name, price, options: [{ label, choices: [{ label, extra }] }] } }, in cents
  const ITEMS = JSON.parse(menu.dataset.items);
  const money = (cents) => `$${(cents / 100).toFixed(2)}`;
  const esc = (s) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
  // A cart line: { id, picks: [the index of the choice of each option], qty }
  let cart = [];
  const unit = (id, picks) => ITEMS[id].price + ITEMS[id].options.reduce((s, o, i) => s + o.choices[picks[i]].extra, 0);
  const optText = (id, picks) => ITEMS[id].options.map((o, i) => o.choices[picks[i]].label).join(", ");

  const renderMenu = () => {
    menu.querySelectorAll("button[data-id]").forEach((b) => {
      const n = cart.filter((l) => l.id === b.dataset.id).reduce((s, l) => s + l.qty, 0);
      let count = b.querySelector(".count");
      if (!n) { if (count) count.remove(); return; }
      if (!count) { count = document.createElement("span"); count.className = "count"; b.append(count); }
      count.textContent = n;
      count.setAttribute("aria-label", `${n} in cart`);
    });
    const count = cart.reduce((s, l) => s + l.qty, 0);
    const sum = cart.reduce((s, l) => s + l.qty * unit(l.id, l.picks), 0);
    document.querySelector(".cartbar").hidden = count === 0;
    document.body.classList.toggle("has-cart", count > 0);
    document.getElementById("cart-sum").textContent = `${count} ${count === 1 ? "item" : "items"} · ${money(sum)}`;
    document.getElementById("confirm-lines").innerHTML = cart.map((l, i) =>
      `<li><button type="button" data-line="${i}"><span>${esc(ITEMS[l.id].name)} ×${l.qty}` +
      (ITEMS[l.id].options.length ? `<small>${esc(optText(l.id, l.picks))}</small>` : "") +
      `</span><span>${money(l.qty * unit(l.id, l.picks))}</span></button></li>`).join("") +
      `<li class="total"><span>Total</span><span>${money(sum)}</span></li>`;
    const fields = document.getElementById("confirm-fields");
    fields.replaceChildren(...cart.map((l) => {
      const input = document.createElement("input");
      input.type = "hidden";
      input.name = "line";
      input.value = `${l.id}:${l.picks.join(",")}:${l.qty}`;
      return input;
    }));
  };

  // The item sheet
  const sheet = document.getElementById("item");
  const confirm = document.getElementById("confirm");
  let editing = null, cur = null, backToConfirm = false;
  const renderSheet = () => {
    const it = ITEMS[cur.id];
    document.getElementById("item-qty").textContent = cur.qty;
    document.getElementById("item-minus").disabled = cur.qty <= 1;
    document.getElementById("item-submit").textContent =
      `${editing === null ? "Add to cart" : "Update"}　${money(cur.qty * unit(cur.id, cur.picks))}`;
    document.getElementById("item-options").innerHTML = it.options.map((o, i) =>
      `<fieldset class="choices"><legend>${esc(o.label)}</legend>` + o.choices.map((c, j) =>
        `<label><input type="radio" name="opt${i}" value="${j}"${cur.picks[i] === j ? " checked" : ""}><span>${esc(c.label)}${c.extra ? ` +${money(c.extra)}` : ""}</span></label>`).join("") +
      `</fieldset>`).join("");
  };
  const openItem = (id, line) => {
    editing = line;
    cur = line === null ? { id, picks: ITEMS[id].options.map(() => 0), qty: 1 } : { ...cart[line], picks: [...cart[line].picks] };
    document.getElementById("item-photo").src = `/static/img/${cur.id}.svg`;
    document.getElementById("item-name").textContent = ITEMS[cur.id].name;
    document.getElementById("item-price").textContent = money(ITEMS[cur.id].price);
    document.getElementById("item-remove").hidden = line === null;
    renderSheet();
    sheet.showModal();
  };
  menu.addEventListener("click", (e) => {
    const b = e.target.closest("button[data-id]");
    if (b) { backToConfirm = false; openItem(b.dataset.id, null); }
  });
  document.getElementById("item-options").addEventListener("change", (e) => {
    cur.picks[Number(e.target.name.slice(3))] = Number(e.target.value);
    renderSheet();
  });
  document.getElementById("item-minus").addEventListener("click", () => { if (cur.qty > 1) { cur.qty--; renderSheet(); } });
  document.getElementById("item-plus").addEventListener("click", () => { if (cur.qty < 99) { cur.qty++; renderSheet(); } });
  const done = () => {
    sheet.close();
    renderMenu();
    if (backToConfirm && cart.length) confirm.showModal();
  };
  document.getElementById("item-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const same = cart.findIndex((l, i) => i !== editing && l.id === cur.id && l.picks.join() === cur.picks.join());
    if (editing !== null) cart.splice(editing, 1, cur); else cart.push(cur);
    if (same !== -1) { // the same item with the same options is one line
      const keep = cart.indexOf(cur);
      cart[same].qty = Math.min(99, cart[same].qty + cur.qty);
      cart.splice(keep, 1);
    }
    done();
  });
  document.getElementById("item-remove").addEventListener("click", () => { cart.splice(editing, 1); done(); });

  // The order sheet
  document.getElementById("open-confirm").addEventListener("click", () => confirm.showModal());
  document.getElementById("confirm-lines").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-line]");
    if (!b) return;
    confirm.close();
    backToConfirm = true;
    openItem(null, Number(b.dataset.line));
  });

  renderMenu();
})();
