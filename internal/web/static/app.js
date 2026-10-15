// Home page behaviour for logged-in users. Loaded with defer, so the DOM is ready.

// Tabs: one panel at a time, selected by the URL fragment (#inbox, #notes, ...).
const tabs = [...document.querySelectorAll('[role=tab]')];
function select(id, focus) {
  const tab = tabs.find(t => t.getAttribute('aria-controls') === id) || tabs[0];
  for (const t of tabs) {
    const on = t === tab;
    t.setAttribute('aria-selected', on);
    t.tabIndex = on ? 0 : -1;
    document.getElementById(t.getAttribute('aria-controls')).hidden = !on;
  }
  if (focus) tab.focus();
}
tabs.forEach((t, i) => {
  t.addEventListener('click', () => {
    history.replaceState(null, '', '#' + t.getAttribute('aria-controls'));
    select(t.getAttribute('aria-controls'));
  });
  t.addEventListener('keydown', e => {
    const step = { ArrowRight: 1, ArrowLeft: -1 }[e.key];
    if (!step) return;
    const next = tabs[(i + step + tabs.length) % tabs.length].getAttribute('aria-controls');
    history.replaceState(null, '', '#' + next);
    select(next, true);
  });
});
addEventListener('hashchange', () => select(location.hash.slice(1)));
select(location.hash.slice(1));

// New note dialog.
const compose = document.getElementById('compose');
document.getElementById('compose-open').addEventListener('click', () => compose.showModal());

// Edit note dialog: each Edit button carries its note's id and text.
const edit = document.getElementById('edit');
for (const b of document.querySelectorAll('[data-edit]')) {
  b.addEventListener('click', () => {
    edit.querySelector('form').action = '/notes/' + b.dataset.edit + '/edit';
    edit.querySelector('textarea').value = b.dataset.text;
    edit.showModal();
  });
}

// Forms that need confirming first, such as Delete.
for (const f of document.querySelectorAll('form[data-confirm]')) {
  f.addEventListener('submit', e => { if (!confirm(f.dataset.confirm)) e.preventDefault(); });
}
