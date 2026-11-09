// Shared image picker for workflow tasks and agent-session prompts.
// Upload only on submit; previews stay local until then. Keep IDs after failed submits.
function imageAttachments(text, zone = text.closest('form') || text.parentElement) {
  const panel = document.createElement('div');
  panel.className = 'attachments';
  const pick = document.createElement('button');
  pick.type = 'button'; pick.className = 'btn'; pick.textContent = 'Attach images';
  const input = document.createElement('input');
  input.type = 'file'; input.multiple = true; input.accept = 'image/png,image/jpeg,image/gif'; input.hidden = true;
  const hint = document.createElement('span'); hint.className = 'hint'; hint.textContent = ' Drop or paste screenshots · up to 8 images, 4 MiB each, 8 MiB total';
  const previews = document.createElement('div'); previews.className = 'attachment-previews';
  const error = document.createElement('p'); error.className = 'warn'; error.setAttribute('role', 'alert');
  panel.append(pick, input, hint, previews, error); text.after(panel);
  let files = [], busy = false, disabled = false;
  const warn = msg => { error.textContent = msg; };
  function add(incoming) {
    if (busy || disabled) return;
    warn('');
    for (const file of incoming) {
      if (!['image/png', 'image/jpeg', 'image/gif'].includes(file.type)) { warn('Use PNG, JPEG or GIF images.'); continue; }
      if (!file.size || file.size > 4 * 1024 * 1024 || files.length >= 8 || files.reduce((n, f) => n + f.file.size, 0) + file.size > 8 * 1024 * 1024) {
        warn('Attach up to 8 images, 4 MiB each and 8 MiB total.'); continue;
      }
      const url = URL.createObjectURL(file), item = { file, url };
      const preview = document.createElement('div'), img = document.createElement('img'), remove = document.createElement('button');
      img.src = url; img.alt = file.name;
      remove.type = 'button'; remove.className = 'btn'; remove.textContent = 'Remove ' + file.name;
      remove.onclick = () => { if (busy || disabled) return; files = files.filter(f => f !== item); URL.revokeObjectURL(url); preview.remove(); };
      preview.append(img, remove); previews.append(preview); files.push(item);
    }
  }
  pick.onclick = () => input.click();
  input.onchange = () => { add(input.files); input.value = ''; };
  zone.addEventListener('dragover', e => { if (e.dataTransfer.types.includes('Files')) { e.preventDefault(); panel.classList.add('dropping'); } });
  zone.addEventListener('dragleave', () => panel.classList.remove('dropping'));
  zone.addEventListener('drop', e => { if (e.dataTransfer.files.length) { e.preventDefault(); panel.classList.remove('dropping'); add(e.dataTransfer.files); } });
  text.addEventListener('paste', e => { if (e.clipboardData.files.length) { e.preventDefault(); add(e.clipboardData.files); } });
  window.addEventListener('pagehide', () => files.forEach(f => URL.revokeObjectURL(f.url)));
  return {
    hasImages: () => files.length > 0,
    setDisabled(value) { disabled = value; pick.disabled = value; },
    clear() {
      files.forEach(f => URL.revokeObjectURL(f.url)); files = [];
      previews.replaceChildren(); input.value = ''; warn('');
    },
    async prompt(value) {
      if (busy) throw new Error('Images are still uploading.');
      busy = true; pick.disabled = true; warn('');
      try {
        for (const item of files) {
          if (item.reference) continue;
          const data = await new Promise((resolve, reject) => {
            const reader = new FileReader(); reader.onerror = () => reject(new Error('Could not read image.'));
            reader.onload = () => resolve(reader.result.split(',')[1]); reader.readAsDataURL(item.file);
          });
          const res = await fetch('/api/attachments', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: item.file.name, data }) });
          if (!res.ok) throw new Error(await res.text());
          item.reference = (await res.json()).reference;
        }
        return value + (files.length ? '\n\nAttached images:\n' + files.map(f => f.reference).join('\n') : '');
      } catch (e) { warn(e.message); throw e; }
      finally { busy = false; pick.disabled = disabled; }
    }
  };
}
