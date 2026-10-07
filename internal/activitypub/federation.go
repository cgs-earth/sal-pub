package activitypub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/httpsig"
	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

const (
	maxBodyBytes    = 1 << 20
	deliveryTimeout = 30 * time.Second
	acceptHeader    = `application/activity+json, application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
)

// inbox accepts Follow and Undo{Follow} from remote actors. Every request
// must carry a valid HTTP Signature from the actor named in the activity.
func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	local, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	signer, err := httpsig.Verify(r, body, s.keyFetcher(local))
	if err != nil {
		log.Printf("inbox %s: rejected: %v", local.Username, err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	item, err := vocab.UnmarshalJSON(body)
	if err != nil {
		http.Error(w, "invalid activity", http.StatusBadRequest)
		return
	}
	act, err := vocab.ToActivity(item)
	if err != nil || act.Actor == nil {
		http.Error(w, "not an activity", http.StatusBadRequest)
		return
	}
	if act.Actor.GetLink().String() != signer {
		http.Error(w, "actor does not match signature", http.StatusUnauthorized)
		return
	}

	switch {
	case act.Match(vocab.FollowType):
		if act.Object == nil || act.Object.GetLink().String() != local.ActorURI {
			http.Error(w, "Follow object is not this actor", http.StatusBadRequest)
			return
		}
		if err := s.acceptFollow(r.Context(), local, signer, act); err != nil {
			util.ServerError(w, err)
			return
		}
		log.Printf("inbox %s: %s followed", local.Username, signer)

	case act.Match(vocab.UndoType):
		// Only an embedded Follow by the same actor counts; a bare IRI could
		// point at anything.
		inner, err := vocab.ToActivity(act.Object)
		if err == nil && inner.Match(vocab.FollowType) &&
			inner.Actor != nil && inner.Actor.GetLink().String() == signer {
			if err := s.store.RemoveFollower(r.Context(), local.ID, signer); err != nil {
				util.ServerError(w, err)
				return
			}
			log.Printf("inbox %s: %s unfollowed", local.Username, signer)
		}

	default:
		log.Printf("inbox %s: ignoring %v from %s", local.Username, act.Type, signer)
	}

	w.WriteHeader(http.StatusAccepted)
}

// acceptFollow records the follower and sends it an Accept.
func (s *Server) acceptFollow(ctx context.Context, local *store.Actor, followerURI string, follow *vocab.Follow) error {
	follower, err := s.fetchActor(ctx, local, followerURI)
	if err != nil {
		return fmt.Errorf("fetch follower: %w", err)
	}
	if follower.ID.String() != followerURI {
		return fmt.Errorf("follower document id %q does not match %q", follower.ID, followerURI)
	}
	if vocab.IsNil(follower.Inbox) {
		return fmt.Errorf("follower %s has no inbox", followerURI)
	}
	personalInbox := follower.Inbox.GetLink().String()
	inbox := personalInbox
	if follower.Endpoints != nil && !vocab.IsNil(follower.Endpoints.SharedInbox) {
		inbox = follower.Endpoints.SharedInbox.GetLink().String()
	}
	if err := s.store.AddFollower(ctx, local.ID, followerURI, inbox); err != nil {
		return err
	}

	acceptActivity := vocab.AcceptNew(vocab.IRI(local.ActorURI+"#accepts/"+rand.Text()), follow)
	acceptActivity.Actor = vocab.IRI(local.ActorURI)
	accept, err := marshal(acceptActivity)
	if err != nil {
		return fmt.Errorf("encode Accept: %w", err)
	}

	// Reply to the follower's own inbox, and do it after this request returns.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
		defer cancel()
		if err := s.deliver(ctx, local, personalInbox, accept); err != nil {
			log.Printf("deliver Accept to %s: %v", personalInbox, err)
		}
	}()
	return nil
}

func (s *Server) deliverToFollowers(a *store.Actor, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()

	inboxes, err := s.store.FollowerInboxes(ctx, a.ID)
	if err != nil {
		log.Printf("list follower inboxes for %s: %v", a.Username, err)
		return
	}
	for _, inbox := range inboxes {
		if err := s.deliver(ctx, a, inbox, payload); err != nil {
			log.Printf("deliver to %s: %v", inbox, err)
		}
	}
}

// deliver POSTs a signed activity to an inbox. There is no retry queue yet.
func (s *Server) deliver(ctx context.Context, from *store.Actor, inbox string, payload []byte) error {
	key, err := httpsig.ParsePrivateKey(from.PrivateKeyPEM)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inbox, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", acceptHeader)
	if err := httpsig.Sign(req, from.PublicKeyID, key, payload); err != nil {
		return err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("post: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// fetchActor GETs and decodes a remote actor document.
func (s *Server) fetchActor(ctx context.Context, local *store.Actor, uri string) (*vocab.Actor, error) {
	body, err := s.fetch(ctx, local, uri)
	if err != nil {
		return nil, err
	}
	item, err := vocab.UnmarshalJSON(body)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", uri, err)
	}
	actor, err := vocab.ToActor(item)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", uri, err)
	}
	return actor, nil
}

// fetch GETs a remote ActivityPub document, signed as local so that servers
// running in "authorized fetch" mode will answer.
func (s *Server) fetch(ctx context.Context, local *store.Actor, uri string) ([]byte, error) {
	key, err := httpsig.ParsePrivateKey(local.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", acceptHeader)
	if err := httpsig.Sign(req, local.PublicKeyID, key, nil); err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s", uri, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", uri, err)
	}
	return body, nil
}

// keyFetcher resolves a keyId by fetching the document it points at, which
// is either the owning actor (keyId is "<actor>#main-key") or a key object.
func (s *Server) keyFetcher(local *store.Actor) httpsig.KeyFetcher {
	return func(ctx context.Context, keyID string) (string, *rsa.PublicKey, error) {
		docURL, _, _ := strings.Cut(keyID, "#")
		body, err := s.fetch(ctx, local, docURL)
		if err != nil {
			return "", nil, err
		}

		var owner, keyPEM string
		var bareKey vocab.PublicKey
		if item, err := vocab.UnmarshalJSON(body); err == nil {
			if actor, err := vocab.ToActor(item); err == nil && actor.PublicKey.ID.String() == keyID {
				owner, keyPEM = actor.ID.String(), actor.PublicKey.PublicKeyPem
			}
		}
		if keyPEM == "" && json.Unmarshal(body, &bareKey) == nil && bareKey.ID.String() == keyID {
			owner, keyPEM = bareKey.Owner.String(), bareKey.PublicKeyPem
		}
		if keyPEM == "" {
			return "", nil, errors.New("document does not contain the key")
		}
		ownerURL, err1 := url.Parse(owner)
		keyURL, err2 := url.Parse(keyID)
		if err1 != nil || err2 != nil || ownerURL.Host == "" || !strings.EqualFold(ownerURL.Host, keyURL.Host) {
			return "", nil, fmt.Errorf("key owner %q is not on the key's host", owner)
		}

		key, err := httpsig.ParsePublicKey(keyPEM)
		if err != nil {
			return "", nil, err
		}
		return owner, key, nil
	}
}
