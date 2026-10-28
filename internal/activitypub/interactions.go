package activitypub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

// Likes, shares and replies of local notes and objects are collections in the
// object store, named after the object:
//
//   - <object>/likes: URIs of the actors that liked it.
//   - <object>/shares: URIs of the actors that announced (boosted) it.
//   - <object>/replies: ids of public notes in reply to it.
//
// A local actor's own Like and Announce activities are stored as objects with
// ids derived from the object they are about (see reactionID), so an actor
// reacts to an object at most once and can undo it by id.

// reactionCollections maps the reaction activity types to the collection of
// the object that counts them.
var reactionCollections = map[vocab.ActivityVocabularyType]string{
	vocab.LikeType:     "likes",
	vocab.AnnounceType: "shares",
}

// reactionType is LikeType or AnnounceType, whichever act (one of the two) is.
func reactionType(act *vocab.Activity) vocab.ActivityVocabularyType {
	if act.Match(vocab.AnnounceType) {
		return vocab.AnnounceType
	}
	return vocab.LikeType
}

// isLocalObject reports whether id is under /notes/ or /objects/ here.
func (s *Server) isLocalObject(id string) bool {
	return strings.HasPrefix(id, s.NoteURI("")) || strings.HasPrefix(id, s.objectURI(""))
}

// setInteractionCollections points a local object's likes, shares and replies
// at its collections.
func setInteractionCollections(obj *vocab.Object) {
	obj.Likes = obj.ID.AddPath("likes")
	obj.Shares = obj.ID.AddPath("shares")
	obj.Replies = obj.ID.AddPath("replies")
}

// canSee reports whether viewer (nil when not logged in) may see obj: it is
// public, or viewer is its author.
func canSee(obj *vocab.Object, viewer *store.Actor) bool {
	return isPublic(obj) ||
		viewer != nil && !vocab.IsNil(obj.AttributedTo) && obj.AttributedTo.GetLink().String() == viewer.ActorURI
}

// reactionID is the id of a's Like or Announce (typ) of objectID.
func reactionID(a *store.Actor, typ vocab.ActivityVocabularyType, objectID string) vocab.IRI {
	sum := sha256.Sum256([]byte(objectID))
	return vocab.IRI(a.ActorURI + "#" + reactionCollections[typ] + "/" + hex.EncodeToString(sum[:16]))
}

// ReplyAddresses returns the to and cc addresses of a's reply to parent, so
// they don't depend on what the user enters: to the public with the parent's
// author in cc if the parent is public, and otherwise to its author only.
func (s *Server) ReplyAddresses(ctx context.Context, a *store.Actor, parent string) (to, cc []string, err error) {
	obj, err := s.loadObject(ctx, a, parent)
	if err != nil {
		return nil, nil, err
	}
	if vocab.IsNil(obj.AttributedTo) {
		return nil, nil, invalid("%s has no author to reply to", parent)
	}
	author := obj.AttributedTo.GetLink().String()
	if !isPublic(obj) {
		return []string{author}, nil, nil
	}
	return []string{store.PublicAddress}, []string{author}, nil
}

// react performs a's Like or Announce (typ) of objectID and returns the
// activity. A local object counts it in its likes or shares collection, and
// the author of a remote object is sent it. An Announce is addressed to the
// public and a's followers, and only public objects can be announced.
// Reacting to the same object again returns the earlier activity.
func (s *Server) react(ctx context.Context, a *store.Actor, typ vocab.ActivityVocabularyType, objectID string) (*vocab.Activity, error) {
	id := reactionID(a, typ, objectID)
	if it, err := s.store.GetObject(ctx, id.String()); err == nil {
		return vocab.ToActivity(it)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	local := s.isLocalObject(objectID)
	obj, err := s.loadObject(ctx, a, objectID)
	if err != nil {
		return nil, err
	}
	if typ == vocab.AnnounceType && !isPublic(obj) {
		return nil, invalid("only public objects can be shared")
	}

	act := vocab.ActivityNew(id, typ, vocab.IRI(objectID))
	act.Actor = vocab.IRI(a.ActorURI)
	act.Published = time.Now().UTC().Truncate(time.Second)
	if !vocab.IsNil(obj.AttributedTo) {
		act.CC = vocab.ItemCollection{obj.AttributedTo.GetLink()}
	}
	if typ == vocab.AnnounceType {
		act.To = vocab.ItemCollection{vocab.PublicNS}
		act.CC = append(act.CC, vocab.IRI(FollowersURI(a)))
	}
	if err := s.store.CreateObject(ctx, act); err != nil {
		return nil, err
	}
	if local {
		if err := s.store.AddToCollection(ctx, objectID+"/"+reactionCollections[typ], a.ActorURI); err != nil {
			return nil, err
		}
	}

	payload, err := marshal(act)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", typ, err)
	}
	// Only the addressing is used; local recipients are skipped.
	go s.deliverNote(a, &vocab.Object{To: act.To, CC: act.CC}, payload, false)
	return act, nil
}

// unreact undoes a's Like or Announce (typ) of objectID and returns the Undo,
// which is sent to the same recipients. It returns ErrNotFound if a hasn't
// reacted to the object that way.
func (s *Server) unreact(ctx context.Context, a *store.Actor, typ vocab.ActivityVocabularyType, objectID string) (*vocab.Activity, error) {
	id := reactionID(a, typ, objectID)
	it, err := s.store.GetObject(ctx, id.String())
	if err != nil {
		return nil, err
	}
	act, err := vocab.ToActivity(it)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", typ, err)
	}
	if err := s.store.DeleteObject(ctx, id.String()); err != nil {
		return nil, err
	}
	if s.isLocalObject(objectID) {
		err := s.store.RemoveFromCollection(ctx, objectID+"/"+reactionCollections[typ], a.ActorURI)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}

	undo := vocab.UndoNew(vocab.IRI(a.ActorURI+"#undos/"+rand.Text()), act)
	undo.Actor = vocab.IRI(a.ActorURI)
	undo.To, undo.CC = act.To, act.CC
	payload, err := marshal(undo)
	if err != nil {
		return nil, fmt.Errorf("encode Undo: %w", err)
	}
	go s.deliverNote(a, &vocab.Object{To: act.To, CC: act.CC}, payload, false)
	return undo, nil
}

// countReaction adds signer to the likes or shares collection of the local
// object that act (a Like or Announce from signer's inbox delivery) is about,
// or with add false removes it. Reactions to objects that aren't local are
// ignored; a missing local object returns ErrNotFound.
func (s *Server) countReaction(ctx context.Context, signer string, act *vocab.Activity, add bool) error {
	if vocab.IsNil(act.Object) {
		return nil
	}
	objectID := act.Object.GetLink().String()
	if !s.isLocalObject(objectID) {
		return nil
	}
	if _, err := s.store.GetObject(ctx, objectID); err != nil {
		return err
	}
	collection := objectID + "/" + reactionCollections[reactionType(act)]
	if add {
		return s.store.AddToCollection(ctx, collection, signer)
	}
	if err := s.store.RemoveFromCollection(ctx, collection, signer); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// addReply lists note, a public reply received from another server, in the
// replies of the local object it is in reply to. Other notes are ignored.
func (s *Server) addReply(ctx context.Context, note *vocab.Note) error {
	if vocab.IsNil(note.InReplyTo) || !isPublic(note) {
		return nil
	}
	parent := note.InReplyTo.GetLink().String()
	if !s.isLocalObject(parent) {
		return nil
	}
	if _, err := s.store.GetObject(ctx, parent); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return s.store.AddToCollection(ctx, parent+"/replies", note.ID.String())
}

// ServeInteractions writes the likes, shares or replies collection of a local
// note or object, at <object>/{collection}, if viewer (nil when not logged
// in) may see the object. Likes and shares only give their count, so who
// reacted isn't published; replies list the ids of public replies.
func (s *Server) ServeInteractions(w http.ResponseWriter, r *http.Request, viewer *store.Actor) {
	name := r.PathValue("collection")
	if name != "likes" && name != "shares" && name != "replies" {
		http.NotFound(w, r)
		return
	}
	collectionID := s.baseURL + r.URL.Path
	it, err := s.loadObject(r.Context(), viewer, strings.TrimSuffix(collectionID, "/"+name))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		util.ServerError(w, err)
		return
	}
	if obj, err := vocab.ToObject(it); err != nil || !canSee(obj, viewer) {
		http.NotFound(w, r)
		return
	}

	ids, err := collect(s.store.CollectionItems(r.Context(), collectionID))
	if err != nil {
		util.ServerError(w, err)
		return
	}
	col := vocab.CollectionNew(vocab.IRI(collectionID))
	col.TotalItems = uint(len(ids))
	if name == "replies" {
		for _, id := range ids {
			col.Items = append(col.Items, vocab.IRI(id))
		}
	}
	writeAP(w, col)
}
