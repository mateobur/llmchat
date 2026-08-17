"use strict";

const $ = (id) => document.getElementById(id);

const el = {
  joinScreen: $("join-screen"),
  joinForm: $("join-form"),
  joinButton: $("join-button"),
  joinError: $("join-error"),
  handle: $("handle"),
  swatches: $("swatches"),
  paletteNote: $("palette-note"),
  customColor: $("custom-color"),
  colorReadout: $("color-readout"),
  accessToken: $("access-token"),
  chatScreen: $("chat-screen"),
  status: $("status"),
  me: $("me"),
  leaveButton: $("leave-button"),
  downloadButton: $("download-button"),
  messages: $("messages"),
  roster: $("roster"),
  rosterCount: $("roster-count"),
  composer: $("composer"),
  text: $("text"),
  mentionMenu: $("mention-menu"),
};

const state = {
  ws: null,
  token: null,
  self: null,
  color: el.customColor.value,
  seen: new Set(), // seq numbers already rendered
  taken: new Set(), // colors in use, for the join screen
  users: new Map(), // lowercase handle -> user, for coloring mentions
  joined: false,
  leaving: false,
  backoff: 500,
  menu: { items: [], index: 0, open: false },
};

// ---------- join screen ----------

async function loadPalette() {
  let data;
  try {
    data = await (await fetch("/api/palette")).json();
  } catch {
    el.paletteNote.textContent = "(server unreachable)";
    return;
  }
  state.taken = new Set(data.taken || []);
  const free = data.free || [];
  el.paletteNote.textContent = `— ${free.length} of ${free.length + state.taken.size} free`;

  el.swatches.replaceChildren();
  for (const color of [...free, ...state.taken].sort()) {
    const taken = state.taken.has(color);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "swatch";
    b.style.background = color;
    b.disabled = taken;
    b.title = taken ? `${color} — taken or too similar to one in use` : color;
    b.setAttribute("aria-pressed", String(color === state.color));
    b.setAttribute("aria-label", color);
    b.addEventListener("click", () => selectColor(color));
    el.swatches.append(b);
  }
  if (!state.color || state.taken.has(state.color)) {
    if (free.length) selectColor(free[0]);
  }
}

function selectColor(color) {
  state.color = color;
  el.customColor.value = color;
  el.colorReadout.textContent = color;
  for (const b of el.swatches.children) {
    b.setAttribute("aria-pressed", String(b.getAttribute("aria-label") === color));
  }
}

el.customColor.addEventListener("input", () => selectColor(el.customColor.value));

el.joinForm.addEventListener("submit", (e) => {
  e.preventDefault();
  const handle = el.handle.value.trim();
  if (!handle) return;
  state.leaving = false;
  showJoinError("");
  el.joinButton.disabled = true;
  el.joinButton.textContent = "Joining…";
  connect({
    type: "join",
    handle,
    color: state.color,
    role: el.joinForm.role.value,
  });
});

function showJoinError(msg) {
  el.joinError.textContent = msg;
  el.joinError.hidden = !msg;
  el.joinButton.disabled = false;
  el.joinButton.textContent = "Join the room";
  if (msg) loadPalette();
}

// ---------- socket ----------

function wsURL(query) {
  const u = new URL("/ws", location.href);
  u.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  for (const [k, v] of Object.entries(query)) if (v) u.searchParams.set(k, v);
  return u.toString();
}

// connect opens the socket. Pass a join frame to claim an identity, or nothing
// to resume the session we already hold a token for.
function connect(joinFrame) {
  const query = state.token
    ? { token: state.token }
    : { access_token: el.accessToken.value.trim() };
  setStatus("connecting…", "");

  const ws = new WebSocket(wsURL(query));
  state.ws = ws;

  ws.addEventListener("open", () => {
    state.backoff = 500;
    if (joinFrame) ws.send(JSON.stringify(joinFrame));
  });

  ws.addEventListener("message", (e) => {
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch {
      return;
    }
    onEvent(ev);
  });

  ws.addEventListener("close", () => {
    setStatus("disconnected", "down");
    el.text.disabled = true;
    if (state.leaving) return;
    if (!state.joined) {
      showJoinError(el.joinError.textContent || "connection refused by the server");
      return;
    }
    // Session survives on the server for a while; resume with the same token.
    const delay = state.backoff;
    state.backoff = Math.min(state.backoff * 2, 10000);
    setStatus(`reconnecting in ${Math.round(delay / 1000) || 1}s`, "down");
    setTimeout(() => connect(), delay);
  });
}

function onEvent(ev) {
  switch (ev.type) {
    case "join":
      if (ev.token) { // our own join was accepted
        state.token = ev.token;
        state.self = ev.self;
        enterChat();
        return;
      }
      appendSystem(ev);
      break;

    case "welcome":
      state.self = ev.self;
      renderRoster(ev.users);
      enterChat();
      setStatus("live", "live");
      el.text.disabled = false;
      break;

    case "message":
      appendMessage(ev);
      break;

    case "leave":
    case "system":
      appendSystem(ev);
      break;

    case "users":
      renderRoster(ev.users);
      break;

    case "error":
      if (!state.joined) {
        showJoinError(ev.error);
      } else if (/not joined/i.test(ev.error)) {
        // The server no longer knows us: it restarted, or the session expired.
        // Reconnecting with the same token would only fail again.
        resetToJoin("The room restarted or your session expired. Join again.");
      } else {
        appendSystem({ ...ev, text: ev.error, type: "error" });
      }
      return;

    case "pong":
      return;
  }

  // Joins and leaves change the roster; ask for a fresh snapshot.
  if (ev.type === "join" || ev.type === "leave") requestUsers();
}

function requestUsers() {
  if (state.ws && state.ws.readyState === WebSocket.OPEN) {
    state.ws.send(JSON.stringify({ type: "users" }));
  }
}

function enterChat() {
  if (state.joined) return;
  state.joined = true;
  el.joinScreen.hidden = true;
  el.chatScreen.hidden = false;
  el.me.textContent = `${state.self.handle} (${state.self.role})`;
  el.me.style.color = state.self.color;
  el.text.focus();
}

// resetToJoin drops a dead session and returns to the join screen, rather than
// retrying a token the server has forgotten.
function resetToJoin(message) {
  state.leaving = true; // stop the reconnect loop
  if (state.ws) state.ws.close();
  state.token = null;
  state.self = null;
  state.joined = false;
  state.seen.clear();
  state.backoff = 500;
  el.messages.replaceChildren();
  el.roster.replaceChildren();
  el.chatScreen.hidden = true;
  el.joinScreen.hidden = false;
  showJoinError(message); // also refreshes the palette
  el.handle.focus();
}

function setStatus(text, cls) {
  el.status.textContent = text;
  el.status.className = "status" + (cls ? " " + cls : "");
}

// ---------- rendering ----------

function atBottom() {
  const m = el.messages;
  return m.scrollHeight - m.scrollTop - m.clientHeight < 60;
}

function append(node, seq) {
  if (seq) {
    if (state.seen.has(seq)) return;
    state.seen.add(seq);
  }
  const stick = atBottom();
  el.messages.append(node);
  if (stick) el.messages.scrollTop = el.messages.scrollHeight;
}

function timeNode(ts) {
  const t = document.createElement("time");
  const d = new Date(ts);
  t.dateTime = ts;
  t.textContent = d.toLocaleTimeString([], { hour12: false });
  return t;
}

// roleMark occupies a column of its own so that handles — and therefore the
// message text — line up whether the speaker is a person or an agent.
function roleMark(user) {
  const mark = document.createElement("span");
  mark.className = "role-mark";
  if (user && user.role === "llm") {
    mark.textContent = "◆";
    mark.title = "LLM agent";
    mark.style.color = user.color;
  }
  return mark;
}

// Mirrors the server's mention syntax. The server decides what counts as a
// mention (ev.mentions); this only needs to find where those tokens sit.
const MENTION_RE = /@([a-zA-Z0-9][a-zA-Z0-9._-]*)/g;
const BROADCAST = new Set(["all", "everyone", "here", "channel"]);

function selfKey() {
  return state.self ? state.self.handle.toLowerCase() : null;
}

function addressesMe(mentions) {
  const me = selfKey();
  if (!me || !mentions) return false;
  return mentions.some((m) => m === me || BROADCAST.has(m));
}

// renderText fills node with the message, wrapping recognised @mentions so they
// stand out in the mentioned participant's own color.
function renderText(node, ev) {
  const mentions = new Set(ev.mentions || []);
  if (!mentions.size) {
    node.textContent = ev.text;
    return;
  }
  let last = 0;
  for (const m of ev.text.matchAll(MENTION_RE)) {
    const raw = m[1].replace(/[._-]+$/, "");
    const key = raw.toLowerCase();
    if (!mentions.has(key)) continue;
    if (m.index > 0 && /[\w@.-]/.test(ev.text[m.index - 1])) continue;

    node.append(document.createTextNode(ev.text.slice(last, m.index)));
    const span = document.createElement("span");
    span.className = "mention";
    span.textContent = "@" + raw;
    const user = state.users.get(key);
    if (user) span.style.color = user.color;
    if (key === selfKey() || BROADCAST.has(key)) span.classList.add("me");
    node.append(span);
    last = m.index + 1 + raw.length;
  }
  node.append(document.createTextNode(ev.text.slice(last)));
}

function appendMessage(ev) {
  const row = document.createElement("div");
  row.className = "msg";
  if (state.self && ev.from && ev.from.handle === state.self.handle) row.classList.add("mine");

  const who = document.createElement("span");
  who.className = "handle";
  who.style.color = ev.from.color;
  who.textContent = ev.from.handle;

  const text = document.createElement("span");
  text.className = "text";
  renderText(text, ev);

  if (addressesMe(ev.mentions)) row.classList.add("tagged");
  row.append(roleMark(ev.from), who, text, timeNode(ev.ts));
  append(row, ev.seq);
}

function appendSystem(ev) {
  const row = document.createElement("div");
  row.className = "sys" + (ev.type === "error" ? " error" : "");

  // The handle keeps its own cell and color, aligned with the message rows.
  const who = document.createElement("span");
  who.className = "handle";
  const text = document.createElement("span");
  text.className = "text";
  if (ev.from) {
    who.style.color = ev.from.color;
    who.textContent = ev.from.handle;
    text.textContent = stripHandle(ev.text, ev.from.handle);
  } else {
    text.textContent = ev.text;
  }

  row.append(roleMark(ev.from), who, text, timeNode(ev.ts));
  append(row, ev.seq);
}

// The server phrases join/leave text as "<handle> joined…"; the handle is
// rendered separately in its own color, so drop the duplicate prefix.
function stripHandle(text, handle) {
  return text.startsWith(handle + " ") ? text.slice(handle.length + 1) : text;
}

function renderRoster(users) {
  if (!users) return;
  state.users = new Map(users.map((u) => [u.handle.toLowerCase(), u]));
  el.rosterCount.textContent = `(${users.length})`;
  el.roster.replaceChildren();
  for (const u of users) {
    const li = document.createElement("li");
    const dot = document.createElement("span");
    dot.className = "dot";
    dot.style.background = u.color;
    const name = document.createElement("span");
    name.textContent = u.handle;
    name.style.color = u.color;
    const role = document.createElement("span");
    role.className = "role";
    role.textContent = u.role;
    li.append(dot, name, role);
    el.roster.append(li);
  }
}

// ---------- @ autocomplete ----------

// The token under the caret, if the caret sits inside one. Mirrors the server's
// idea of where a mention may start: after nothing, whitespace or an opening
// bracket, so an e-mail address never opens the menu.
const TOKEN_RE = /(?:^|[\s(\[{])@([a-zA-Z0-9._-]*)$/;

function activeMention() {
  const caret = el.text.selectionStart;
  const m = el.text.value.slice(0, caret).match(TOKEN_RE);
  if (!m) return null;
  return { prefix: m[1], start: caret - m[1].length - 1, end: caret };
}

// Candidates are the people in the room, then the broadcast keywords.
function mentionCandidates(prefix) {
  const key = prefix.toLowerCase();
  const me = selfKey();
  const people = [];
  for (const [handleKey, u] of state.users) {
    if (handleKey !== me && handleKey.startsWith(key)) {
      people.push({ handle: u.handle, color: u.color, note: u.role });
    }
  }
  people.sort((a, b) => a.handle.localeCompare(b.handle));

  const broadcasts = [...BROADCAST]
    .filter((b) => b.startsWith(key))
    .sort()
    .map((b) => ({ handle: b, color: null, note: "everyone" }));

  return [...people, ...broadcasts].slice(0, 8);
}

function refreshMentionMenu() {
  const active = activeMention();
  const items = active ? mentionCandidates(active.prefix) : [];
  if (!items.length) {
    closeMentionMenu();
    return;
  }
  state.menu = { items, index: 0, open: true };
  drawMentionMenu();
}

function drawMentionMenu() {
  el.mentionMenu.replaceChildren();
  state.menu.items.forEach((item, i) => {
    const li = document.createElement("li");
    li.id = "mention-option-" + i;
    li.setAttribute("role", "option");
    li.setAttribute("aria-selected", String(i === state.menu.index));

    const dot = document.createElement("span");
    dot.className = "dot" + (item.color ? "" : " broadcast");
    if (item.color) dot.style.background = item.color;

    const name = document.createElement("span");
    name.textContent = "@" + item.handle;
    if (item.color) name.style.color = item.color;

    const note = document.createElement("span");
    note.className = "note";
    note.textContent = item.note;

    li.append(dot, name, note);
    // mousedown, not click: it fires before the input loses focus.
    li.addEventListener("mousedown", (e) => {
      e.preventDefault();
      acceptMention(item);
    });
    el.mentionMenu.append(li);
  });
  el.mentionMenu.hidden = false;
  el.text.setAttribute("aria-expanded", "true");
  el.text.setAttribute("aria-activedescendant", "mention-option-" + state.menu.index);
}

function closeMentionMenu() {
  state.menu = { items: [], index: 0, open: false };
  el.mentionMenu.hidden = true;
  el.mentionMenu.replaceChildren();
  el.text.setAttribute("aria-expanded", "false");
  el.text.removeAttribute("aria-activedescendant");
}

function moveMentionSelection(delta) {
  const n = state.menu.items.length;
  state.menu.index = (state.menu.index + delta + n) % n;
  drawMentionMenu();
}

function acceptMention(item) {
  const active = activeMention();
  if (!active) return closeMentionMenu();
  const value = el.text.value;
  const insert = "@" + item.handle + " ";
  el.text.value = value.slice(0, active.start) + insert + value.slice(active.end);
  const caret = active.start + insert.length;
  el.text.setSelectionRange(caret, caret);
  closeMentionMenu();
}

el.text.addEventListener("input", refreshMentionMenu);
el.text.addEventListener("click", refreshMentionMenu);
el.text.addEventListener("blur", () => setTimeout(closeMentionMenu, 120));

el.text.addEventListener("keydown", (e) => {
  if (!state.menu.open) return;
  switch (e.key) {
    case "ArrowDown":
      e.preventDefault();
      moveMentionSelection(1);
      break;
    case "ArrowUp":
      e.preventDefault();
      moveMentionSelection(-1);
      break;
    case "Enter":
    case "Tab":
      // The menu owns Enter while it is open, so a half-typed handle is
      // completed instead of being sent.
      e.preventDefault();
      acceptMention(state.menu.items[state.menu.index]);
      break;
    case "Escape":
      e.preventDefault();
      closeMentionMenu();
      break;
  }
});

// ---------- composer ----------

el.composer.addEventListener("submit", (e) => {
  e.preventDefault();
  const text = el.text.value;
  if (!text.trim() || !state.ws || state.ws.readyState !== WebSocket.OPEN) return;
  state.ws.send(JSON.stringify({ type: "message", text }));
  el.text.value = "";
  closeMentionMenu();
});

// The transcript needs the session token, so it cannot be a plain link: fetch
// it, then hand the blob to a synthetic download.
el.downloadButton.addEventListener("click", async () => {
  if (!state.token) return;
  const button = el.downloadButton;
  const label = button.textContent;
  button.disabled = true;
  button.textContent = "Saving…";
  try {
    const resp = await fetch("/api/transcript", {
      headers: { Authorization: "Bearer " + state.token },
    });
    if (!resp.ok) throw new Error((await resp.json()).error || resp.statusText);
    const blob = await resp.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filenameFrom(resp.headers.get("Content-Disposition")) || "conversation.json";
    a.click();
    URL.revokeObjectURL(url);
  } catch (err) {
    appendSystem({ type: "error", ts: new Date().toISOString(), text: "download failed: " + err.message });
  } finally {
    button.disabled = false;
    button.textContent = label;
  }
});

// The server names the file; this only reads that name back out of the header.
function filenameFrom(disposition) {
  const m = /filename="([^"]+)"/.exec(disposition || "");
  return m ? m[1] : null;
}

el.leaveButton.addEventListener("click", () => {
  state.leaving = true;
  if (state.ws && state.ws.readyState === WebSocket.OPEN) {
    state.ws.send(JSON.stringify({ type: "leave" }));
    state.ws.close();
  }
  location.reload();
});

window.addEventListener("beforeunload", () => {
  state.leaving = true;
  if (state.ws && state.ws.readyState === WebSocket.OPEN) {
    state.ws.send(JSON.stringify({ type: "leave" }));
  }
});

loadPalette();
el.handle.focus();
