package activitypub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"iter"
	"log"
	"net/url"
	"strings"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
)

// Follow state lives in the generic object store:
//
//   - <actor>/followers: collection of follower actor URIs.
//   - <actor>/following: collection of actor URIs whose follow was accepted.
//   - <actor>#follows: private collection of the ids of Follow activities the
//     actor sent, pending or accepted. Each Follow is stored as an object.
//   - Remote actor documents (followers and follow targets) are cached as
//     objects, so delivery can find their inboxes.

// FollowingURI returns the IRI of an actor's following collection.
func FollowingURI(a *store.Actor) string {
	return a.ActorURI + "/following"
}

func followsID(a *store.Actor) string {
	return a.ActorURI + "#follows"
}

// FollowStatus is an actor that a local actor follows or has asked to follow.
type FollowStatus struct {
	TargetURI string
	Accepted  bool
}

// collect drains a collection iterator into a slice.
func collect(items iter.Seq2[string, error]) ([]string, error) {
	var out []string
	for id, err := range items {
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// putObject stores it, replacing any stored object with the same id.
func (s *Server) putObject(ctx context.Context, it vocab.Item) error {
	err := s.store.CreateObject(ctx, it)
	if errors.Is(err, store.ErrExists) {
		return s.store.UpdateObject(ctx, it)
	}
	return err
}

// Followers returns the URIs of a's followers, most recent first.
func (s *Server) Followers(ctx context.Context, a *store.Actor) ([]string, error) {
	return collect(s.store.CollectionItems(ctx, FollowersURI(a)))
}

// Following returns everyone a follows or has asked to follow, most recent first.
func (s *Server) Following(ctx context.Context, a *store.Actor) ([]FollowStatus, error) {
	var out []FollowStatus
	for id, err := range s.store.CollectionItems(ctx, followsID(a)) {
		if err != nil {
			return nil, err
		}
		follow, err := s.getFollow(ctx, id)
		if err != nil {
			log.Printf("following of %s: %v", a.Username, err)
			continue
		}
		target := follow.Object.GetLink().String()
		accepted, err := s.store.CollectionContains(ctx, FollowingURI(a), target)
		if err != nil {
			return nil, err
		}
		out = append(out, FollowStatus{TargetURI: target, Accepted: accepted})
	}
	return out, nil
}

func (s *Server) getFollow(ctx context.Context, id string) (*vocab.Activity, error) {
	it, err := s.store.GetObject(ctx, id)
	if err != nil {
		return nil, err
	}
	follow, err := vocab.ToActivity(it)
	if err != nil || !follow.Match(vocab.FollowType) || vocab.IsNil(follow.Object) {
		return nil, fmt.Errorf("object %s is not a Follow", id)
	}
	return follow, nil
}

// findFollow returns the Follow that local sent to targetURI, or ErrNotFound.
func (s *Server) findFollow(ctx context.Context, local *store.Actor, targetURI string) (*vocab.Activity, error) {
	for id, err := range s.store.CollectionItems(ctx, followsID(local)) {
		if err != nil {
			return nil, err
		}
		if follow, err := s.getFollow(ctx, id); err == nil && follow.Object.GetLink().String() == targetURI {
			return follow, nil
		}
	}
	return nil, store.ErrNotFound
}

// dropFollow forgets a Follow that local sent, and the follow it granted.
func (s *Server) dropFollow(ctx context.Context, local *store.Actor, follow *vocab.Activity) error {
	id, target := follow.ID.String(), follow.Object.GetLink().String()
	for _, err := range []error{
		s.store.RemoveFromCollection(ctx, followsID(local), id),
		s.store.DeleteObject(ctx, id),
		s.store.RemoveFromCollection(ctx, FollowingURI(local), target),
	} {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// acceptFollowing records that the signer accepted the Follow followID. It
// returns ErrNotFound unless that Follow was sent by local to the signer.
func (s *Server) acceptFollowing(ctx context.Context, local *store.Actor, signer, followID string) error {
	sent, err := s.store.CollectionContains(ctx, followsID(local), followID)
	if err != nil {
		return err
	}
	if !sent {
		return store.ErrNotFound
	}
	follow, err := s.getFollow(ctx, followID)
	if err != nil {
		return err
	}
	if follow.Object.GetLink().String() != signer {
		return store.ErrNotFound
	}
	return s.store.AddToCollection(ctx, FollowingURI(local), signer)
}

// followerInboxes returns the inboxes to deliver a's activities to: each
// follower's shared inbox if it has one, otherwise its inbox. Local followers
// get their local inbox URL; remote actor documents are read from the cache,
// or fetched and cached.
func (s *Server) followerInboxes(ctx context.Context, a *store.Actor) []string {
	var inboxes []string
	for follower, err := range s.store.CollectionItems(ctx, FollowersURI(a)) {
		if err != nil {
			log.Printf("list followers of %s: %v", a.Username, err)
			break
		}
		if strings.HasPrefix(follower, s.baseURL+"/users/") {
			inboxes = append(inboxes, follower+"/inbox")
			continue
		}
		actor, err := s.cachedActor(ctx, a, follower)
		if err != nil {
			log.Printf("follower %s of %s: %v", follower, a.Username, err)
			continue
		}
		inboxes = append(inboxes, deliveryInbox(actor))
	}
	return inboxes
}

// cachedActor returns a remote actor document from the object store, or
// fetches it (signed as local) and caches it.
func (s *Server) cachedActor(ctx context.Context, local *store.Actor, uri string) (*vocab.Actor, error) {
	if it, err := s.store.GetObject(ctx, uri); err == nil {
		if actor, err := vocab.ToActor(it); err == nil && !vocab.IsNil(actor.Inbox) {
			return actor, nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	actor, err := s.fetchActor(ctx, local, uri)
	if err != nil {
		return nil, err
	}
	if vocab.IsNil(actor.Inbox) {
		return nil, fmt.Errorf("%s has no inbox", uri)
	}
	if err := s.putObject(ctx, actor); err != nil {
		return nil, err
	}
	return actor, nil
}

// deliveryInbox is an actor's shared inbox if it advertises one, else its inbox.
func deliveryInbox(actor *vocab.Actor) string {
	if actor.Endpoints != nil && !vocab.IsNil(actor.Endpoints.SharedInbox) {
		return actor.Endpoints.SharedInbox.GetLink().String()
	}
	return actor.Inbox.GetLink().String()
}

// Follow asks the actor named by ref (an actor URL or a @user@host handle) to
// accept local as a follower, and returns the Follow. Its object is the
// target's actor URI. Remote
// targets get a signed Follow and stay pending until they send an Accept;
// local targets are followed directly. Following again replaces the earlier
// request.
func (s *Server) Follow(ctx context.Context, local *store.Actor, ref string) (*vocab.Activity, error) {
	targetURI, err := s.resolveActorURI(ctx, ref)
	if err != nil {
		return nil, err
	}
	if targetURI == local.ActorURI {
		return nil, errors.New("you can't follow yourself")
	}

	var target *vocab.Actor
	if username, ok := strings.CutPrefix(targetURI, s.baseURL+"/users/"); ok {
		if _, err := s.store.GetByUsername(ctx, username); errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("no local user %q", username)
		} else if err != nil {
			return nil, err
		}
	} else {
		if target, err = s.fetchActor(ctx, local, targetURI); err != nil {
			return nil, fmt.Errorf("fetch %s: %w", targetURI, err)
		}
		// The Accept will be signed by the actor's own id, so use that. It
		// must be on the host we fetched it from.
		fetched, _ := url.Parse(targetURI)
		canonical, err := url.Parse(target.ID.String())
		if err != nil || !strings.EqualFold(canonical.Host, fetched.Host) {
			return nil, fmt.Errorf("actor id %q is not on %s", target.ID, fetched.Host)
		}
		if vocab.IsNil(target.Inbox) {
			return nil, fmt.Errorf("%s has no inbox", target.ID)
		}
		targetURI = target.ID.String()
		if err := s.putObject(ctx, target); err != nil {
			return nil, err
		}
	}

	if old, err := s.findFollow(ctx, local, targetURI); err == nil {
		if err := s.dropFollow(ctx, local, old); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	follow := vocab.FollowNew(vocab.IRI(local.ActorURI+"#follows/"+rand.Text()), vocab.IRI(targetURI))
	follow.Actor = vocab.IRI(local.ActorURI)
	// Record it before sending, so a fast Accept finds it.
	if err := s.store.CreateObject(ctx, follow); err != nil {
		return nil, err
	}
	if err := s.store.AddToCollection(ctx, followsID(local), follow.ID.String()); err != nil {
		return nil, err
	}

	if target == nil { // local: accepted immediately
		if err := s.store.AddToCollection(ctx, targetURI+"/followers", local.ActorURI); err != nil {
			return nil, err
		}
		return follow, s.store.AddToCollection(ctx, FollowingURI(local), targetURI)
	}

	payload, err := marshal(follow)
	if err != nil {
		return nil, fmt.Errorf("encode Follow: %w", err)
	}
	if err := s.deliver(ctx, local, target.Inbox.GetLink().String(), payload); err != nil {
		if dropErr := s.dropFollow(ctx, local, follow); dropErr != nil {
			log.Printf("remove failed follow of %s: %v", targetURI, dropErr)
		}
		return nil, fmt.Errorf("send Follow to %s: %w", target.Inbox.GetLink(), err)
	}
	return follow, nil
}

// Unfollow stops local following targetURI and returns the Undo{Follow}, or
// returns ErrNotFound. Remote targets are sent the Undo; a failed delivery is
// logged, and the follow is still removed.
func (s *Server) Unfollow(ctx context.Context, local *store.Actor, targetURI string) (*vocab.Activity, error) {
	follow, err := s.findFollow(ctx, local, targetURI)
	if err != nil {
		return nil, err
	}
	if err := s.dropFollow(ctx, local, follow); err != nil {
		return nil, err
	}
	undo := vocab.UndoNew(vocab.IRI(local.ActorURI+"#undos/"+rand.Text()), follow)
	undo.Actor = vocab.IRI(local.ActorURI)

	if strings.HasPrefix(targetURI, s.baseURL+"/users/") {
		err := s.store.RemoveFromCollection(ctx, targetURI+"/followers", local.ActorURI)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		return undo, nil
	}

	target, err := s.cachedActor(ctx, local, targetURI)
	if err != nil {
		log.Printf("send Undo{Follow} to %s: %v", targetURI, err)
		return undo, nil
	}
	payload, err := marshal(undo)
	if err != nil {
		return nil, fmt.Errorf("encode Undo: %w", err)
	}
	if err := s.deliver(ctx, local, target.Inbox.GetLink().String(), payload); err != nil {
		log.Printf("send Undo{Follow} to %s: %v", targetURI, err)
	}
	return undo, nil
}
