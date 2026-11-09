// ⌘ Enter (macOS) or Ctrl Enter sends from any text box, on every page (the layout loads this).
// What it presses is the box's nearest form or [data-submit] ancestor:
//   form           its default (first) submit button, through form.requestSubmit, so the form's own
//                  submit handler and validation run as they would for a click
//   [data-submit]  for a box outside a form: a click on the button its selector names
// A disabled button sends nothing. Plain Enter is left alone (a textarea keeps its newline), as is
// Shift, ⌥ or a held key, and a box with its own ⌘ Enter (the agent chat) that prevents the default.
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" || !(e.metaKey || e.ctrlKey) || e.shiftKey || e.altKey || e.isComposing || e.repeat || e.defaultPrevented) return;
  const box = e.target && e.target.closest && e.target.closest("textarea, input, select, [contenteditable]");
  const host = box && box.closest("form, [data-submit]");
  if (!host) return;
  e.preventDefault();
  if (host.tagName !== "FORM") {
    const b = document.querySelector(host.dataset.submit);
    if (b && !b.disabled) b.click();
    return;
  }
  const b = [...host.elements].find((el) => el.type === "submit");
  if (b && b.disabled) return;
  host.requestSubmit(b);
});
