package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cgs-earth/sal-pub/internal/activitypub"
	"github.com/cgs-earth/sal-pub/internal/store"
)

func render(t *testing.T, data map[string]any) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, homeTemplate.Execute(&b, data))
	return b.String()
}

func loggedInData() map[string]any {
	actor := &store.Actor{Username: "alice", DisplayName: "Alice", ActorURI: "https://sal.example/users/alice"}
	followers := actor.ActorURI + "/followers"
	return map[string]any{
		"Actor":     actor,
		"Handle":    "@alice@sal.example",
		"NoteCount": 1,
		"Notes": []activitypub.Note{{
			ID: "n1", Content: "<p>hi</p>", Published: time.Now(),
			To: []string{store.PublicAddress}, CC: []string{followers},
		}},
		"Inbox":        []store.InboxNote{{ObjectID: "https://r.example/n/1", AttributedTo: "https://r.example/u/bob", Content: "<p>hello <script>x</script></p>"}},
		"Following":    []activitypub.FollowStatus{{TargetURI: "https://r.example/u/bob", Accepted: false}},
		"Followers":    []string{"https://r.example/u/carol"},
		"MaxNoteChars": maxNoteChars,
		"DefaultTo":    "public",
		"DefaultCC":    followers,
	}
}

func TestHomeLoggedOutShowsLoginAndPublicPosts(t *testing.T) {
	out := render(t, map[string]any{"PublicNotes": []activitypub.Note{{ID: "n1", Content: "<p>pub</p>", Author: &store.Actor{Username: "bob"}}}})
	for _, want := range []string{`href="/login"`, "Public posts", "<p>pub</p>"} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, `role="tablist"`, "logged-out page should have no tabs")
	assert.NotContains(t, out, "<dialog", "logged-out page should have no compose dialog")
}

func TestHomeLoggedInHasTabsAndComposeDialog(t *testing.T) {
	out := render(t, loggedInData())
	for _, want := range []string{
		`role="tablist"`, `aria-controls="inbox"`, `aria-controls="notes"`, `aria-controls="following"`,
		`<dialog id="compose"`, `action="/notes"`, `id="compose-open"`, `<span class="pill warn">pending</span>`,
	} {
		assert.Contains(t, out, want)
	}
}

func TestHomeShowsAddressesAsPills(t *testing.T) {
	out := render(t, loggedInData())
	assert.Contains(t, out, `<span class="pill">public</span>`)
	assert.Contains(t, out, `<span class="pill">followers</span>`)
}

func TestHomeEscapesInboxContent(t *testing.T) {
	out := render(t, loggedInData())
	assert.NotContains(t, out, "<script>x</script>", "remote inbox HTML must not be rendered as HTML")
}

func TestStaticAssetsServedUnderStatic(t *testing.T) {
	srv := http.FileServerFS(staticFS)
	for path, want := range map[string]string{"/static/app.css": "--accent", "/static/app.js": "showModal"} {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, w.Code, "GET %s", path)
		assert.Contains(t, w.Body.String(), want, "GET %s", path)
	}
}

func TestHomeSentNotesStartWithAddresses(t *testing.T) {
	out := render(t, loggedInData())
	assert.Contains(t, out, `<article class="card">
        <div class="meta">to <span class="pill">public</span>`, "a sent note should start with its to/cc line, with no author line")
}

func TestHomeAccountListsShowFullURIs(t *testing.T) {
	out := render(t, loggedInData())
	assert.Contains(t, out, `<a class="uri" href="https://r.example/u/carol">https://r.example/u/carol</a>`,
		"followers should show the full URI, including the scheme")
}

func TestHomeNoteTimeIsAFooter(t *testing.T) {
	out := render(t, loggedInData())
	assert.Contains(t, out, `<footer><a href="/notes/n1"><time datetime="`, "a sent note's time should be a footer linking to the note")
	assert.Contains(t, out, `<footer><a href="https://r.example/n/1"><time datetime="`, "an inbox note's time should be a footer linking to the note")
}

func TestHomeOutboxNotesHaveEditAndDelete(t *testing.T) {
	out := render(t, loggedInData())
	for _, want := range []string{
		`data-edit="n1" data-text="hi"`, `action="/notes/n1/delete" data-confirm=`, `<dialog id="edit"`,
	} {
		assert.Contains(t, out, want)
	}
}

func TestHomeMarksEditedNotes(t *testing.T) {
	data := loggedInData()
	data["Notes"].([]activitypub.Note)[0].Updated = time.Now()
	assert.Contains(t, render(t, data), ">edited</span>", "an edited note should say so")
}

func TestHomeEditTextReversesNoteHTML(t *testing.T) {
	data := loggedInData()
	data["Notes"].([]activitypub.Note)[0].Content = "<p>a &amp; b<br>c</p><p>d</p>"
	// html/template escapes the text again for the attribute; newlines stay as they are.
	assert.Contains(t, render(t, data), "data-text=\"a &amp; b\nc\n\nd\"", "the Edit button should carry the note's original text")
}
