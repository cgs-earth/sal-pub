package activitypub

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

// HTTP CRUD for generic objects at /objects/{id}. The server assigns ids and
// sets attributedTo to the logged-in actor; only that actor may change or
// delete an object. Objects not addressed to the public are only visible to
// their author. Changes are stored, not federated.

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

// ownObject loads /objects/{id} and checks that a is its author, writing a
// 404 or 403 if not.
func (s *Server) ownObject(w http.ResponseWriter, r *http.Request, a *store.Actor) (*vocab.Object, bool) {
	it, err := s.store.GetObject(r.Context(), s.objectURI(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	} else if err != nil {
		util.ServerError(w, err)
		return nil, false
	}
	obj, err := vocab.ToObject(it)
	if err != nil {
		util.ServerError(w, fmt.Errorf("decode object: %w", err))
		return nil, false
	}
	if vocab.IsNil(obj.AttributedTo) || obj.AttributedTo.GetLink().String() != a.ActorURI {
		http.Error(w, "you can only change your own objects", http.StatusForbidden)
		return nil, false
	}
	return obj, true
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
	public := obj.To.Contains(vocab.PublicNS) || obj.CC.Contains(vocab.PublicNS)
	isAuthor := viewer != nil && !vocab.IsNil(obj.AttributedTo) && obj.AttributedTo.GetLink().String() == viewer.ActorURI
	if !public && !isAuthor {
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
	obj.ID = vocab.IRI(s.objectURI(rand.Text()))
	obj.AttributedTo = vocab.IRI(a.ActorURI)
	obj.Published = time.Now().UTC().Truncate(time.Second)
	obj.Updated = time.Time{}
	if err := s.store.CreateObject(r.Context(), obj); err != nil {
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
	existing, ok := s.ownObject(w, r, a)
	if !ok {
		return
	}
	obj, err := decodeObject(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	obj.ID = existing.ID
	obj.AttributedTo = existing.AttributedTo
	obj.Published = existing.Published
	obj.Updated = time.Now().UTC().Truncate(time.Second)
	if err := s.store.UpdateObject(r.Context(), obj); err != nil {
		util.ServerError(w, err)
		return
	}
	writeAP(w, obj)
}

// ServeDeleteObject deletes /objects/{id} and replies 204.
func (s *Server) ServeDeleteObject(w http.ResponseWriter, r *http.Request, a *store.Actor) {
	obj, ok := s.ownObject(w, r, a)
	if !ok {
		return
	}
	if err := s.store.DeleteObject(r.Context(), obj.ID.String()); err != nil {
		util.ServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
