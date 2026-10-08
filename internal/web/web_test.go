package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cgs-earth/sal-pub/internal/activitypub"
	"github.com/cgs-earth/sal-pub/internal/store"
)

func render(t *testing.T, data map[string]any) string {
	t.Helper()
	var b strings.Builder
	if err := homeTemplate.Execute(&b, data); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func loggedInData() map[string]any {
	actor := &store.Actor{Username: "alice", DisplayName: "Alice", ActorURI: "https://sal.example/users/alice"}
	followers := actor.ActorURI + "/followers"
	return map[string]any{
		"Actor":     actor,
		"Handle":    "@alice@sal.example",
		"NoteCount": 1,
		"Notes": []store.Note{{
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
	out := render(t, map[string]any{"PublicNotes": []store.PublicNote{{Username: "bob", Note: store.Note{ID: "n1", Content: "<p>pub</p>"}}}})
	for _, want := range []string{`href="/login"`, "Public posts", "<p>pub</p>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, `role="tablist"`) || strings.Contains(out, "<dialog") {
		t.Error("logged-out page should have no tabs or compose dialog")
	}
}

func TestHomeLoggedInHasTabsAndComposeDialog(t *testing.T) {
	out := render(t, loggedInData())
	for _, want := range []string{
		`role="tablist"`, `aria-controls="inbox"`, `aria-controls="notes"`, `aria-controls="following"`,
		`<dialog id="compose"`, `action="/notes"`, `id="compose-open"`, `<span class="pill warn">pending</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestHomeShowsAddressesAsPills(t *testing.T) {
	out := render(t, loggedInData())
	if !strings.Contains(out, `<span class="pill">public</span>`) || !strings.Contains(out, `<span class="pill">followers</span>`) {
		t.Error("public and followers addresses should render as pills")
	}
}

func TestHomeEscapesInboxContent(t *testing.T) {
	out := render(t, loggedInData())
	if strings.Contains(out, "<script>x</script>") {
		t.Error("remote inbox HTML must not be rendered as HTML")
	}
}

func TestStaticAssetsServedUnderStatic(t *testing.T) {
	srv := http.FileServerFS(staticFS)
	for path, want := range map[string]string{"/static/app.css": "--accent", "/static/app.js": "showModal"} {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("GET %s: %d, missing %q", path, w.Code, want)
		}
	}
}

func TestHomeSentNotesStartWithAddresses(t *testing.T) {
	out := render(t, loggedInData())
	if !strings.Contains(out, `<article class="card">
        <div class="meta">to <span class="pill">public</span>`) {
		t.Error("a sent note should start with its to/cc line, with no author line")
	}
}

func TestHomeAccountListsShowFullURIs(t *testing.T) {
	out := render(t, loggedInData())
	if !strings.Contains(out, `<a class="uri" href="https://r.example/u/carol">https://r.example/u/carol</a>`) {
		t.Error("followers should show the full URI, including the scheme")
	}
}

func TestHomeNoteTimeIsAFooter(t *testing.T) {
	out := render(t, loggedInData())
	if !strings.Contains(out, `<footer><a href="/notes/n1"><time datetime="`) {
		t.Error("a sent note's time should be a footer linking to the note")
	}
	if !strings.Contains(out, `<footer><a href="https://r.example/n/1"><time datetime="`) {
		t.Error("an inbox note's time should be a footer linking to the note")
	}
}
