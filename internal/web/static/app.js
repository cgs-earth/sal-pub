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

// New note dialog. Reply buttons open it as a reply to their object, with the
// object's author added to CC so they receive it.
const compose = document.getElementById('compose');
const composeCC = document.getElementById('compose-cc');
const replyLine = document.getElementById('compose-reply');
function openCompose(parent, author) {
  compose.querySelector('[name=in_reply_to]').value = parent || '';
  document.getElementById('compose-title').textContent = parent ? 'Reply' : 'New note';
  replyLine.hidden = !parent;
  replyLine.querySelector('a').href = parent || '';
  replyLine.querySelector('a').textContent = parent || '';
  composeCC.value = [author].filter(Boolean).join(' ');
  compose.showModal();
}
document.getElementById('compose-open').addEventListener('click', () => openCompose());
for (const b of document.querySelectorAll('[data-reply]')) {
  b.addEventListener('click', () => openCompose(b.dataset.reply, b.dataset.author));
}

// Editing, deleting and liking post an activity to the user's outbox, then
// reload the page. Errors are shown in an alert.
const outbox = document.getElementById('notes').dataset.outbox;
async function postActivity(activity) {
  const res = await fetch(outbox, {
    method: 'POST',
    headers: { 'Content-Type': 'application/activity+json' },
    body: JSON.stringify({ '@context': 'https://www.w3.org/ns/activitystreams', ...activity }),
  });
  if (!res.ok) {
    alert(await res.text() || res.statusText);
    return;
  }
  location.reload();
}

// Edit note dialog: each Edit button carries its note's id and text.
const edit = document.getElementById('edit');
const editForm = edit.querySelector('form');
for (const b of document.querySelectorAll('[data-edit]')) {
  b.addEventListener('click', () => {
    editForm.dataset.note = b.dataset.edit;
    edit.querySelector('textarea').value = b.dataset.text;
    edit.showModal();
  });
}
editForm.addEventListener('submit', e => {
  if (e.submitter && e.submitter.formMethod === 'dialog') return;
  e.preventDefault();
  postActivity({
    type: 'Update',
    object: {
      id: editForm.dataset.note, type: 'Note',
      source: { content: edit.querySelector('textarea').value, mediaType: 'text/plain' },
    },
  });
});

document.getElementById('edit-delete').addEventListener('click', () => {
  if (confirm("Delete this note? Its recipients' servers are asked to delete it too.")) {
    postActivity({ type: 'Delete', object: editForm.dataset.note });
  }
});

// Like buttons like their object, or undo the like when it is pressed.
for (const b of document.querySelectorAll('[data-like]')) {
  b.addEventListener('click', () => {
    const like = { type: 'Like', object: b.dataset.like };
    postActivity(b.getAttribute('aria-pressed') === 'true' ? { type: 'Undo', object: like } : like);
  });
}
