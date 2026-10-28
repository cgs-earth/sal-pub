package activitypub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

// Client-to-server posting: a logged-in user POSTs an activity to their own
// outbox and the server performs it on their behalf. Supported:
//
//   - Create{Note}: published as a local note and delivered (see notes.go).
//   - Create{other object}: stored at /objects/{id}, not federated.
//   - Update and Delete of the user's own notes and objects (objects.go).
//   - Follow and Undo{Follow}.
//   - Like and Announce of any object, and Undo of them (interactions.go).
//
// A Create{Note} may be inReplyTo another object, which must exist and be
// visible to the user if it is local.
//
// A bare object is treated as Create{object}. The browser forms go through
// the same code (PostOutbox).

// MaxNoteChars is the longest note text accepted.
const MaxNoteChars = 5000

// invalid returns a util.ErrBadRequest with a message for the client.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", util.ErrBadRequest, fmt.Sprintf(format, args...))
}

// ServePostOutbox performs the activity in the request body as a and replies
// 201 with the activity and its id in Location. The caller must have checked
// that the request is from a.
func (s *Server) ServePostOutbox(w http.ResponseWriter, r *http.Request, a *store.Actor) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	it, err := vocab.UnmarshalJSON(body)
	if err != nil || vocab.IsNil(it) {
		http.Error(w, "body must be an ActivityStreams activity or object", http.StatusBadRequest)
		return
	}
	act, err := s.PostOutbox(r.Context(), a, it)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	data, err := marshal(act)
	if err != nil {
		util.ServerError(w, fmt.Errorf("encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Location", act.ID.String())
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

// PostOutbox performs it (an activity, or a bare object to create) as a and
// returns the activity as performed, with its server-assigned id. Errors wrap
// util.ErrBadRequest, ErrForbidden, ErrNotFound or ErrBadGateway, or are
// internal; util.ServerError replies with the matching status.
func (s *Server) PostOutbox(ctx context.Context, a *store.Actor, it vocab.Item) (*vocab.Activity, error) {
	if vocab.ObjectTypes.Match(it.GetType()) {
		it = vocab.CreateNew("", it)
	}
	if !vocab.ActivityTypes.Match(it.GetType()) {
		return nil, invalid("body must be an activity or an object such as a Note")
	}
	act, err := vocab.ToActivity(it)
	if err != nil {
		return nil, invalid("%v", err)
	}
	if !vocab.IsNil(act.Actor) && act.Actor.GetLink().String() != a.ActorURI {
		return nil, fmt.Errorf("%w: actor must be %s", util.ErrForbidden, a.ActorURI)
	}
	if vocab.IsNil(act.Object) {
		return nil, invalid("%s needs an object", act.Type)
	}

	switch {
	case act.Match(vocab.CreateType):
		obj, err := vocab.ToObject(act.Object)
		if err != nil || !vocab.ObjectTypes.Match(obj.Type) {
			return nil, invalid("Create needs an embedded object such as a Note")
		}
		to, err := recipients(obj.To, act.To)
		if err != nil {
			return nil, err
		}
		cc, err := recipients(obj.CC, act.CC)
		if err != nil {
			return nil, err
		}
		if obj.Match(vocab.NoteType) {
			text, err := noteText(obj)
			if err != nil {
				return nil, err
			}
			if len(to) == 0 && len(cc) == 0 {
				return nil, invalid("a note needs at least one recipient in to or cc")
			}
			var inReplyTo string
			if !vocab.IsNil(obj.InReplyTo) {
				inReplyTo = obj.InReplyTo.GetLink().String()
				if u, err := url.Parse(inReplyTo); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
					return nil, invalid("inReplyTo must be an http(s) URL")
				}
				it, err := s.loadObject(ctx, a, inReplyTo)
				if parent, perr := vocab.ToObject(it); err != nil || perr != nil || !canSee(parent, a) {
					return nil, invalid("inReplyTo %s not found", inReplyTo)
				}
			}
			note, err := s.PublishNote(ctx, a, text, to, cc, inReplyTo)
			if err != nil {
				return nil, err
			}
			return s.createActivity(a, note), nil
		}
		obj.To, obj.CC = nil, nil
		for _, iri := range to {
			obj.To = append(obj.To, vocab.IRI(iri))
		}
		for _, iri := range cc {
			obj.CC = append(obj.CC, vocab.IRI(iri))
		}
		if err := s.createObject(ctx, a, obj); err != nil {
			return nil, err
		}
		create := vocab.CreateNew(obj.ID+"#create", obj)
		create.Actor = vocab.IRI(a.ActorURI)
		create.Published = obj.Published
		create.To, create.CC = obj.To, obj.CC
		return create, nil

	case act.Match(vocab.UpdateType):
		obj, err := vocab.ToObject(act.Object)
		if err != nil || !vocab.ObjectTypes.Match(obj.Type) || vocab.IsNil(obj.ID) {
			return nil, invalid("Update needs an embedded object with an id")
		}
		return s.updateObject(ctx, a, obj)

	case act.Match(vocab.DeleteType):
		return s.deleteObject(ctx, a, act.Object.GetLink().String())

	case act.Match(vocab.FollowType):
		ref := strings.TrimSpace(act.Object.GetLink().String())
		follow, err := s.Follow(ctx, a, ref)
		if err != nil {
			return nil, fmt.Errorf("%w: could not follow %s: %v", util.ErrBadGateway, ref, err)
		}
		return follow, nil

	case act.Match(vocab.LikeType, vocab.AnnounceType):
		return s.react(ctx, a, reactionType(act), act.Object.GetLink().String())

	case act.Match(vocab.UndoType):
		// The activity to undo, by id or embedded.
		inner, err := vocab.ToActivity(act.Object)
		if vocab.IsIRI(act.Object) {
			it, getErr := s.store.GetObject(ctx, act.Object.GetLink().String())
			if getErr != nil {
				return nil, getErr
			}
			if inner, err = vocab.ToActivity(it); err == nil &&
				(vocab.IsNil(inner.Actor) || inner.Actor.GetLink().String() != a.ActorURI) {
				return nil, fmt.Errorf("%w: not your activity", util.ErrForbidden)
			}
		}
		if err != nil || vocab.IsNil(inner.Object) {
			return nil, invalid("only Follow, Like and Announce can be undone")
		}
		switch {
		case inner.Match(vocab.FollowType):
			return s.Unfollow(ctx, a, inner.Object.GetLink().String())
		case inner.Match(vocab.LikeType, vocab.AnnounceType):
			return s.unreact(ctx, a, reactionType(inner), inner.Object.GetLink().String())
		}
		return nil, invalid("only Follow, Like and Announce can be undone")
	}
	return nil, invalid("%s is not supported", act.Type)
}

// recipients merges to or cc lists into validated, de-duplicated IRIs.
func recipients(lists ...vocab.ItemCollection) ([]string, error) {
	var all []string
	for _, l := range lists {
		for _, it := range l {
			all = append(all, it.GetLink().String())
		}
	}
	out, err := ParseAddresses(strings.Join(all, " "))
	if err != nil {
		return nil, invalid("%v", err)
	}
	return out, nil
}

// noteText returns the text of a note posted by a client: its source when
// that is plain text, otherwise its content with the HTML removed. Notes are
// always re-rendered from text, so clients can't inject HTML.
func noteText(note *vocab.Object) (string, error) {
	var text string
	if c := note.Source.Content.First(); c != nil && note.Source.MediaType == "text/plain" {
		text = c.String()
	} else if c := note.Content.First(); c != nil {
		text = HTMLToText(c.String())
	}
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > MaxNoteChars {
		return "", invalid("a note must be 1-%d characters", MaxNoteChars)
	}
	return text, nil
}
