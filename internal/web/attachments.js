// Shared file and image picker for workflow tasks, replies, and agent-session prompts.
// Upload only on submit; previews stay local until then. Keep IDs after failed submits.
function imageAttachments(text, zone = text.closest('form') || text.parentElement, options = {}) {
  const imageTypes = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
  const panel = options.panel || document.createElement('div');
  panel.className = 'attachments';
  const pick = document.createElement('button');
  pick.type = 'button'; pick.className = 'btn'; pick.textContent = '＋ Attach';
  pick.title = 'Attach images or files. Up to 8 files, 4 MiB each, 8 MiB total.';
  pick.setAttribute('aria-label', 'Attach images or files');
  const input = document.createElement('input');
  input.type = 'file'; input.multiple = true; input.hidden = true;
  const hint = document.createElement('details'); hint.className = 'attachment-help';
  const summary = document.createElement('summary'); summary.textContent = 'File limits';
  const limits = document.createElement('span'); limits.textContent = 'Images or files · up to 8 files, 4 MiB each, 8 MiB total. Drop or paste into the box.';
  hint.append(summary, limits);
  const previews = document.createElement('div'); previews.className = 'attachment-previews';
  const error = document.createElement('p'); error.className = 'warn'; error.setAttribute('role', 'alert');
  const actions = document.createElement('span'); actions.className = 'attachment-actions'; actions.append(pick, input, hint);
  panel.append(previews, error);
  if (options.action) options.action.append(actions); else { panel.prepend(actions); if (!options.panel) text.after(panel); }
  let files = [], busy = false, disabled = false;
  const warn = msg => { error.textContent = msg; };
  const fileBadge = name => {
    const dot = name.lastIndexOf('.');
    const ext = dot > 0 ? name.slice(dot + 1).replace(/[^a-zA-Z0-9]/g, '').toUpperCase().slice(0, 4) : '';
    return ext || 'FILE';
  };
  const fileName = file => {
    const raw = (file.name || '').trim();
    if (raw) return raw;
    const ext = { 'image/png': 'png', 'image/jpeg': 'jpg', 'image/gif': 'gif', 'image/webp': 'webp' }[file.type];
    return ext ? 'pasted-image.' + ext : 'pasted-file';
  };
  function add(incoming) {
    if (busy || disabled) return;
    warn('');
    for (const file of incoming) {
      if (!file.size || file.size > 4 * 1024 * 1024 || files.length >= 8 || files.reduce((n, f) => n + f.file.size, 0) + file.size > 8 * 1024 * 1024) {
        warn('Attach up to 8 files, 1 byte to 4 MiB each and 8 MiB total.'); continue;
      }
      const isImage = imageTypes.includes(file.type);
      const label = fileName(file);
      const url = isImage ? URL.createObjectURL(file) : '';
      const item = { file, name: label, url, isImage };
      const preview = document.createElement('div'), remove = document.createElement('button'), name = document.createElement('span');
      preview.className = 'attachment-chip';
      let thumb;
      if (isImage) {
        thumb = document.createElement('img'); thumb.src = url; thumb.alt = label;
      } else {
        thumb = document.createElement('span'); thumb.className = 'attachment-file'; thumb.textContent = fileBadge(label); thumb.setAttribute('aria-hidden', 'true');
      }
      name.className = 'attachment-name'; name.textContent = label; name.title = label;
      remove.type = 'button'; remove.className = 'btn ghost attachment-remove'; remove.textContent = '×';
      remove.setAttribute('aria-label', 'Remove ' + label); remove.title = 'Remove ' + label;
      remove.onclick = () => { if (busy || disabled) return; files = files.filter(f => f !== item); if (url) URL.revokeObjectURL(url); preview.remove(); pick.focus(); };
      preview.append(thumb, name, remove); previews.append(preview); files.push(item);
    }
  }
  const clipboardFiles = cd => {
    if (!cd) return [];
    if (cd.files && cd.files.length) return cd.files;
    if (cd.items) return Array.from(cd.items).filter(i => i.kind === 'file').map(i => i.getAsFile()).filter(Boolean);
    return [];
  };
  pick.onclick = () => input.click();
  input.onchange = () => { add(input.files); input.value = ''; };
  zone.addEventListener('dragover', e => { if (e.dataTransfer.types.includes('Files')) { e.preventDefault(); zone.classList.add('dropping'); } });
  zone.addEventListener('dragleave', e => { if (!zone.contains(e.relatedTarget)) zone.classList.remove('dropping'); });
  zone.addEventListener('drop', e => { zone.classList.remove('dropping'); if (e.dataTransfer.files.length) { e.preventDefault(); add(e.dataTransfer.files); } });
  text.addEventListener('paste', e => { const pasted = clipboardFiles(e.clipboardData); if (pasted.length) { e.preventDefault(); add(pasted); } });
  window.addEventListener('pagehide', () => files.forEach(f => { if (f.url) URL.revokeObjectURL(f.url); }));
  return {
    hasImages: () => files.length > 0,
    hasFiles: () => files.length > 0,
    setDisabled(value) { disabled = value; pick.disabled = value; previews.querySelectorAll('button').forEach(b => b.disabled = value); },
    clear() {
      files.forEach(f => { if (f.url) URL.revokeObjectURL(f.url); }); files = [];
      previews.replaceChildren(); input.value = ''; warn('');
    },
    async prompt(value) {
      if (busy) throw new Error('Files are still uploading.');
      busy = true; pick.disabled = true; warn('');
      try {
        for (const item of files) {
          if (item.reference) continue;
          const data = await new Promise((resolve, reject) => {
            const reader = new FileReader(); reader.onerror = () => reject(new Error('Could not read file.'));
            reader.onload = () => resolve(reader.result.split(',')[1]); reader.readAsDataURL(item.file);
          });
          const res = await fetch('/api/attachments', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: item.name, data }) });
          if (!res.ok) throw new Error(await res.text());
          item.reference = (await res.json()).reference;
        }
        if (!files.length) return value;
        const heading = files.every(f => f.isImage) ? 'Attached images:' : 'Attached files:';
        return (value ? value + '\n\n' : '') + heading + '\n' + files.map(f => f.reference).join('\n');
      } finally { busy = false; pick.disabled = disabled; }
    }
  };
}
