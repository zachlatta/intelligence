// Dictation (⌘D, Ctrl+D or the mic button) into a textarea: the microphone streams to AssemblyAI's
// streaming speech-to-text with a short-lived token from the server (POST tokenPath), and the words
// come into the box as they are said. With text already in the box they go in at the cursor and
// stay there; in an empty box, stopping sends them (⌘D, Enter or the send button), Edit leaves them
// to change first, and Cancel (Esc) drops them.
//
// dictation({ text, tokenPath, mic, row, label, send, warn }):
//   text      the textarea
//   tokenPath where to POST for a streaming token
//   mic       the button that starts and stops dictation
//   row       the element the dictation bar stands in for while dictating (hidden meanwhile)
//   label()   the send button's name now ("Send", "Start agent")
//   send()    sends what the box holds
//   warn(msg) shows a problem
window.dictation = ({ text, tokenPath, mic, row, label, send, warn }) => {
  const bar = document.createElement('div');
  bar.className = 'dictation';
  bar.hidden = true;
  bar.innerHTML = '<span class="dstatus"><span class="rec"></span><span class="dtext">Listening…</span></span>' +
    '<button type="button" class="btn sm" data-act="cancel" title="Stop and discard (Esc)">Cancel</button>' +
    '<button type="button" class="btn sm" data-act="edit" title="Stop and leave the words in the box to edit">Edit</button>' +
    '<button type="button" class="btn sm primary" data-act="send"><span></span><kbd>⌘D</kbd></button>';
  row.after(bar);
  const status = bar.querySelector('.dtext'), editButton = bar.querySelector('[data-act=edit]');
  const sendButton = bar.querySelector('[data-act=send]');
  const tap = 'class Tap extends AudioWorkletProcessor { process(inputs) { const c = inputs[0] && inputs[0][0];' +
    ' if (c) this.port.postMessage(c.slice(0)); return true; } } registerProcessor("tap", Tap);';
  let session = null;

  const words = s => [...s.turns.keys()].sort((a, b) => a - b).map(k => s.turns.get(k).trim()).filter(Boolean).join(' ');
  function render(s) {
    const said = words(s);
    const sep = said && s.before && !/\s$/.test(s.before) ? ' ' : '';
    const tail = said && s.after && !/^\s/.test(s.after) ? ' ' : '';
    text.value = s.before + sep + said + tail + s.after;
    const at = (s.before + sep + said).length;
    text.setSelectionRange(at, at);
    text.dispatchEvent(new Event('input'));
  }
  function show(s) {
    const on = !!s;
    bar.hidden = !on; row.hidden = on;
    text.readOnly = on; text.classList.toggle('listening', on);
    if (!on) return;
    bar.classList.remove('finishing');
    status.textContent = 'Starting…';
    editButton.hidden = s.mode === 'append';
    sendButton.firstChild.textContent = s.mode === 'append' ? 'Done' : label();
    sendButton.title = s.mode === 'append' ? 'Stop dictating (⌘D or Enter)' : 'Stop and send (⌘D or Enter)';
  }
  function release(s) {
    s.stream?.getTracks().forEach(t => t.stop());
    s.ctx?.close().catch(() => {});
    s.stream = s.ctx = null;
  }

  async function start() {
    const mode = text.value.trim() ? 'append' : 'send';
    const s = session = { mode, original: text.value, before: text.value.slice(0, text.selectionStart),
      after: text.value.slice(text.selectionEnd), turns: new Map(), queue: [], ws: null, stopping: false };
    let closed; s.closed = new Promise(r => closed = r);
    show(s);
    try {
      const token = fetch(tokenPath, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).then(async res => {
        if (res.status === 401) { location.href = '/login?next=' + encodeURIComponent(location.pathname); throw new Error('signed out'); }
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        return res.json();
      });
      token.catch(() => {});
      s.stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
      if (session !== s) return release(s);
      const ctx = s.ctx = new AudioContext();
      const mod = URL.createObjectURL(new Blob([tap], { type: 'application/javascript' }));
      await ctx.audioWorklet.addModule(mod);
      URL.revokeObjectURL(mod);
      if (session !== s) return release(s);
      // 16-bit PCM in 50ms chunks, queued until the stream opens so nothing said early is lost
      const per = Math.round(ctx.sampleRate / 20);
      let buf = new Int16Array(per), n = 0;
      const push = chunk => s.ws?.readyState === WebSocket.OPEN ? s.ws.send(chunk) : s.queue.length < 1200 && s.queue.push(chunk);
      s.flush = () => { if (n) push(buf.slice(0, n).buffer); n = 0; };
      const node = new AudioWorkletNode(ctx, 'tap');
      node.port.onmessage = e => {
        if (!s.stream) return;
        for (const v of e.data) {
          buf[n++] = Math.max(-32768, Math.min(32767, Math.round(v * 32768)));
          if (n === per) { push(buf.buffer); buf = new Int16Array(per); n = 0; }
        }
      };
      ctx.createMediaStreamSource(s.stream).connect(node);
      node.connect(ctx.destination); // silent; keeps the node pulled
      status.textContent = 'Connecting…';
      const { token: t, url } = await token;
      if (s.mode === 'cancel') return release(s);
      const ws = s.ws = new WebSocket(url + '&sample_rate=' + ctx.sampleRate + '&token=' + encodeURIComponent(t));
      ws.binaryType = 'arraybuffer';
      ws.onopen = () => {
        s.queue.forEach(c => ws.send(c)); s.queue = [];
        if (s.stopping) s.end(); else status.textContent = 'Listening…';
      };
      ws.onmessage = e => {
        const m = JSON.parse(e.data);
        if (m.type === 'Turn') { s.turns.set(m.turn_order, m.transcript || ''); if (s.mode !== 'cancel') render(s); }
        else if (m.type === 'Termination') closed();
      };
      ws.onclose = e => {
        if (!s.stopping && e.code !== 1000) { warn('Dictation: ' + (e.reason || 'the stream closed (' + e.code + ')')); finish(s, 'edit'); }
        closed();
      };
      ws.onerror = () => {};
    } catch (err) {
      closed();
      if (session !== s) return;
      warn('Dictation: ' + (err.name === 'NotAllowedError' ? 'the browser was not allowed to use the microphone' : err.message));
      release(s); session = null; text.value = s.original; show(null); text.focus();
    }
  }

  // finish stops the microphone, waits for the last words, then sends, keeps (edit, done) or drops them.
  async function finish(s, action) {
    if (s.stopping) return;
    s.stopping = true;
    s.flush?.();
    release(s);
    if (action === 'cancel') {
      s.mode = 'cancel';
      s.ws?.close();
    } else {
      bar.classList.add('finishing');
      status.textContent = 'Finishing…';
      s.end = () => { try { s.ws.send(JSON.stringify({ type: 'ForceEndpoint' })); s.ws.send(JSON.stringify({ type: 'Terminate' })); } catch {} };
      if (s.ws?.readyState === WebSocket.OPEN) s.end();
      await Promise.race([s.closed, new Promise(r => setTimeout(r, 8000))]);
      s.ws?.close();
    }
    if (session === s) session = null;
    if (action === 'cancel') text.value = s.original; else render(s);
    text.dispatchEvent(new Event('input'));
    show(null);
    text.focus();
    if (action === 'send') {
      if (words(s)) send();
      else warn('Dictation: heard nothing');
    }
  }

  const toggle = () => session ? finish(session, session.mode === 'append' ? 'done' : 'send') : start();
  mic.addEventListener('click', toggle);
  bar.addEventListener('click', e => {
    const a = e.target.closest('[data-act]')?.dataset.act;
    if (a && session) finish(session, a === 'send' && session.mode === 'append' ? 'done' : a);
  });
  // capture, so these win over the page's own keys while dictating
  document.addEventListener('keydown', e => {
    if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'd') {
      e.preventDefault(); e.stopImmediatePropagation(); toggle(); return;
    }
    if (!session || e.isComposing) return;
    if (e.key === 'Escape') { e.preventDefault(); e.stopImmediatePropagation(); finish(session, 'cancel'); }
    else if (e.key === 'Enter') { e.preventDefault(); e.stopImmediatePropagation(); toggle(); }
  }, true);
};
