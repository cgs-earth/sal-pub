package activitypub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

// HTTP CRUD for generic objects at /objects/{id}. The server assigns ids and
// sets attributedTo to the logged-in actor; only that actor may change or
// delete an object. Objects not addressed to the public are only visible to
// their author. Changes are stored, not federated. The same operations are
// available as activities posted to the outbox (see outbox.go).

func (s *Server) objectURI(id string) string {
	return s.baseURL + "/objects/" + id
}

// decodeObject reads a request body holding a plain ActivityStreams object
// (Note, Article, Image, ...), not an activity, actor or collection.
func decodeObject(w http.ResponseWriter, r *http.Request) (*vocab.Object, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, errors.New("request body too large")
	}
	it, err := vocab.UnmarshalJSON(body)
	if err != nil || vocab.IsNil(it) || !vocab.ObjectTypes.Match(it.GetType()) {
		return nil, errors.New("body must be an ActivityStreams object such as a Note")
	}
	return vocab.ToObject(it)
}

// ownObject loads the object id and checks that a is its author. It returns
// store.ErrNotFound or util.ErrForbidden if not.
func (s *Server) ownObject(ctx context.Context, a *store.Actor, id string) (*vocab.Object, error) {
	it, err := s.store.GetObject(ctx, id)
	if err != nil {
		return nil, err
	}
	obj, err := vocab.ToObject(it)
	if err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	if vocab.IsNil(obj.AttributedTo) || obj.AttributedTo.GetLink().String() != a.ActorURI {
		return nil, fmt.Errorf("%w: you can only change your own objects", util.ErrForbidden)
	}
	return obj, nil
}

// createObject stores obj under a new id, attributed to a.
func (s *Server) createObject(ctx context.Context, a *store.Actor, obj *vocab.Object) error {
	obj.ID = vocab.IRI(s.objectURI(rand.Text()))
	setInteractionCollections(obj)
	obj.AttributedTo = vocab.IRI(a.ActorURI)
	obj.Published = time.Now().UTC().Truncate(time.Second)
	obj.Updated = time.Time{}
	return s.store.CreateObject(ctx, obj)
}

// updateObject replaces a's object obj.ID by obj and returns the Update
// activity. The author, type and published time are kept and updated is set
// to now. A local note (under /notes/) keeps its addressing and only its text
// changes: content is re-rendered from it, local inbox copies are updated and
// the Update is delivered to the note's recipients.
func (s *Server) updateObject(ctx context.Context, a *store.Actor, obj *vocab.Object) (*vocab.Activity, error) {
	existing, err := s.ownObject(ctx, a, obj.ID.String())
	if err != nil {
		return nil, err
	}
	if obj.Type != existing.Type {
		return nil, invalid("an Update can't change an object's type from %s", existing.Type)
	}
	isNote := strings.HasPrefix(existing.ID.String(), s.NoteURI(""))
	if isNote {
		text, err := noteText(obj)
		if err != nil {
			return nil, err
		}
		existing.Content = vocab.DefaultNaturalLanguage(textToHTML(text))
		obj = existing
	}
	setInteractionCollections(obj)
	obj.AttributedTo = existing.AttributedTo
	obj.Published = existing.Published
	obj.Updated = time.Now().UTC().Truncate(time.Second)
	if err := s.store.UpdateObject(ctx, obj); err != nil {
		return nil, err
	}

	update := vocab.UpdateNew(vocab.IRI(fmt.Sprintf("%s#update-%d", obj.ID, obj.Updated.Unix())), obj)
	update.Actor = vocab.IRI(a.ActorURI)
	update.Published = obj.Updated
	if isNote {
		update.To, update.CC = obj.To, obj.CC
		if err := s.store.UpdateInboxNotes(ctx, obj.ID.String(), obj.Content.First().String()); err != nil {
			return nil, err
		}
		payload, err := marshal(update)
		if err != nil {
			return nil, fmt.Errorf("encode Update: %w", err)
		}
		go s.deliverNote(a, obj, payload, false)
	}
	return update, nil
}

// deleteObject deletes a's object id and returns the Delete activity, whose
// object is a Tombstone. A local note (under /notes/) is also removed from its
// collections and from local inboxes, and the Delete is delivered to the
// note's recipients.
func (s *Server) deleteObject(ctx context.Context, a *store.Actor, id string) (*vocab.Activity, error) {
	obj, err := s.ownObject(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteObject(ctx, id); err != nil {
		return nil, err
	}

	tombstone := &vocab.Tombstone{
		ID: obj.ID, Type: vocab.TombstoneType, FormerType: obj.Type,
		Deleted: time.Now().UTC().Truncate(time.Second),
	}
	del := vocab.DeleteNew(obj.ID+"#delete", tombstone)
	del.Actor = vocab.IRI(a.ActorURI)
	del.Published = tombstone.Deleted
	if strings.HasPrefix(id, s.NoteURI("")) {
		del.To, del.CC = obj.To, obj.CC
		for _, c := range s.noteCollections(a, obj) {
			if err := s.store.RemoveFromCollection(ctx, c, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
		if err := s.store.DeleteInboxNotes(ctx, id); err != nil {
			return nil, err
		}
		payload, err := marshal(del)
		if err != nil {
			return nil, fmt.Errorf("encode Delete: %w", err)
		}
		go s.deliverNote(a, obj, payload, false)
	}
	return del, nil
}

// ServeObject writes /objects/{id}. viewer is the logged-in actor, or nil.
func (s *Server) ServeObject(w http.ResponseWriter, r *http.Request, viewer *store.Actor) {
	it, err := s.store.GetObject(r.Context(), s.objectURI(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		util.ServerError(w, err)
		return
	}
	obj, err := vocab.ToObject(it)
	if err != nil {
		util.ServerError(w, fmt.Errorf("decode object: %w", err))
		return
	}
	if !canSee(obj, viewer) {
		http.NotFound(w, r)
		return
	}
	writeAP(w, obj)
}

// ServeCreateObject stores the object in the request body under a new id,
// attributed to a, and replies 201 with the stored object.
func (s *Server) ServeCreateObject(w http.ResponseWriter, r *http.Request, a *store.Actor) {
	obj, err := decodeObject(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.createObject(r.Context(), a, obj); err != nil {
		util.ServerError(w, err)
		return
	}

	data, err := marshal(obj)
	if err != nil {
		util.ServerError(w, fmt.Errorf("encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Location", obj.ID.String())
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

// ServeUpdateObject replaces /objects/{id} with the request body. The id,
// author and published time are kept; updated is set to now.
func (s *Server) ServeUpdateObject(w http.ResponseWriter, r *http.Request, a *store.Actor) {
	obj, err := decodeObject(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	obj.ID = vocab.IRI(s.objectURI(r.PathValue("id")))
	update, err := s.updateObject(r.Context(), a, obj)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	writeAP(w, update.Object)
}

// ServeDeleteObject deletes /objects/{id} and replies 204.
func (s *Server) ServeDeleteObject(w http.ResponseWriter, r *http.Request, a *store.Actor) {
	if _, err := s.deleteObject(r.Context(), a, s.objectURI(r.PathValue("id"))); err != nil {
		util.ServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
