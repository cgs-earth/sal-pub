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
	"slices"
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

// inbox accepts Follow, Undo{Follow}, Accept/Reject of our own Follows,
// Create{Note}, and Like, Announce and their Undo from remote actors. Every request must carry a valid HTTP Signature from
// the actor named in the activity.
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

	case act.Match(vocab.LikeType, vocab.AnnounceType):
		err := s.countReaction(r.Context(), signer, act, true)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			util.ServerError(w, err)
			return
		}
		log.Printf("inbox %s: %v of %v from %s", local.Username, act.Type, act.Object.GetLink(), signer)

	case act.Match(vocab.UndoType):
		// Only an embedded activity by the same actor counts; a bare IRI could
		// point at anything.
		inner, err := vocab.ToActivity(act.Object)
		if err != nil || inner.Actor == nil || inner.Actor.GetLink().String() != signer {
			break
		}
		switch {
		case inner.Match(vocab.FollowType):
			err = s.store.RemoveFromCollection(r.Context(), FollowersURI(local), signer)
			log.Printf("inbox %s: %s unfollowed", local.Username, signer)
		case inner.Match(vocab.LikeType, vocab.AnnounceType):
			err = s.countReaction(r.Context(), signer, inner, false)
			log.Printf("inbox %s: %s undid %v of %v", local.Username, signer, inner.Type, inner.Object.GetLink())
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			util.ServerError(w, err)
			return
		}

	case act.Match(vocab.AcceptType):
		// Only accepts the pending Follow we sent to the signer, matched by id.
		if act.Object == nil {
			http.Error(w, "Accept has no object", http.StatusBadRequest)
			return
		}
		err := s.acceptFollowing(r.Context(), local, signer, act.Object.GetLink().String())
		if errors.Is(err, store.ErrNotFound) {
			log.Printf("inbox %s: ignoring Accept from %s for an unknown Follow", local.Username, signer)
		} else if err != nil {
			util.ServerError(w, err)
			return
		} else {
			log.Printf("inbox %s: %s accepted our follow", local.Username, signer)
		}

	case act.Match(vocab.CreateType):
		if err := s.receiveNote(r.Context(), local, signer, act); err != nil {
			log.Printf("inbox %s: ignoring Create from %s: %v", local.Username, signer, err)
		}

	case act.Match(vocab.RejectType):
		follow, err := s.findFollow(r.Context(), local, signer)
		if err == nil {
			err = s.dropFollow(r.Context(), local, follow)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			util.ServerError(w, err)
			return
		}
		log.Printf("inbox %s: %s rejected our follow", local.Username, signer)

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
	if err := s.putObject(ctx, follower); err != nil {
		return err
	}
	if err := s.store.AddToCollection(ctx, FollowersURI(local), followerURI); err != nil {
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

// receiveNote stores the Note in a Create sent to local's inbox. The note must
// be embedded, attributed to the signer and hosted on the signer's server, and
// local must follow the signer or be addressed directly.
func (s *Server) receiveNote(ctx context.Context, local *store.Actor, signer string, act *vocab.Activity) error {
	if vocab.IsNil(act.Object) || vocab.IsIRI(act.Object) {
		return errors.New("object is not embedded")
	}
	obj, err := vocab.ToObject(act.Object)
	if err != nil || !obj.Match(vocab.NoteType) {
		return errors.New("object is not a Note")
	}
	if obj.AttributedTo == nil || obj.AttributedTo.GetLink().String() != signer {
		return errors.New("note is not attributed to the signer")
	}
	objURL, err1 := url.Parse(obj.ID.String())
	signerURL, err2 := url.Parse(signer)
	if err1 != nil || err2 != nil || !strings.EqualFold(objURL.Host, signerURL.Host) {
		return fmt.Errorf("note id %q is not on the signer's host", obj.ID)
	}

	me := vocab.IRI(local.ActorURI)
	if !obj.To.Contains(me) && !obj.CC.Contains(me) && !act.To.Contains(me) && !act.CC.Contains(me) {
		following, err := s.store.CollectionContains(ctx, FollowingURI(local), signer)
		if err != nil {
			return err
		}
		if !following {
			return errors.New("not following the sender and not addressed")
		}
	}

	published := obj.Published
	if published.IsZero() {
		published = time.Now()
	}
	var content string
	if c := obj.Content.First(); c != nil {
		content = c.String()
	}
	if err := s.store.AddInboxNote(ctx, local.ID, store.InboxNote{
		ObjectID: obj.ID.String(), AttributedTo: signer, Content: content, Published: published,
	}); err != nil {
		return err
	}
	if err := s.addReply(ctx, obj); err != nil {
		return err
	}
	log.Printf("inbox %s: stored note %s from %s", local.Username, obj.ID, signer)
	return nil
}

// resolveActorURI turns an actor URL or a @user@host handle into an actor
// URI. Local handles are resolved directly; remote ones through WebFinger.
func (s *Server) resolveActorURI(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.Contains(ref, "://") {
		if u, err := url.Parse(ref); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("invalid actor URL %q", ref)
		}
		return ref, nil
	}

	user, host, ok := strings.Cut(strings.TrimPrefix(ref, "@"), "@")
	if !ok || user == "" || host == "" {
		return "", fmt.Errorf("%q is not a handle like @user@example.org or an actor URL", ref)
	}
	if strings.EqualFold(host, s.domain) {
		return s.baseURL + "/users/" + strings.ToLower(user), nil
	}

	query := url.Values{"resource": {"acct:" + user + "@" + host}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://"+host+"/.well-known/webfinger?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build WebFinger request: %w", err)
	}
	req.Header.Set("Accept", "application/jrd+json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("WebFinger %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("WebFinger %s: %s", ref, resp.Status)
	}

	var jrd struct {
		Links []struct{ Rel, Type, Href string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&jrd); err != nil {
		return "", fmt.Errorf("decode WebFinger %s: %w", ref, err)
	}
	for _, l := range jrd.Links {
		if l.Rel == "self" && (l.Type == contentType || strings.HasPrefix(l.Type, "application/ld+json")) {
			return l.Href, nil
		}
	}
	return "", fmt.Errorf("WebFinger %s: no ActivityPub actor link", ref)
}

// deliverNote sends an activity about a note (payload) to everyone the note is
// addressed to: follower inboxes when the actor's followers collection is
// addressed, and the inbox (or shared inbox) of every other addressed actor.
// Each inbox gets it once. With storeLocal (a Create), the note is written
// straight into local recipients' inboxes; otherwise local inboxes are skipped,
// since edits and deletes update local copies directly.
func (s *Server) deliverNote(a *store.Actor, note *vocab.Note, payload []byte, storeLocal bool) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()

	var inboxes []string
	for _, it := range slices.Concat(note.To, note.CC) {
		switch addr := it.GetLink().String(); addr {
		case store.PublicAddress, a.ActorURI:
			continue
		case FollowersURI(a):
			inboxes = append(inboxes, s.followerInboxes(ctx, a)...)
		default:
			if strings.HasPrefix(addr, s.baseURL+"/users/") {
				inboxes = append(inboxes, addr+"/inbox")
				continue
			}
			recipient, err := s.cachedActor(ctx, a, addr)
			if err != nil {
				log.Printf("resolve recipient %s: %v", addr, err)
				continue
			}
			inboxes = append(inboxes, deliveryInbox(recipient))
		}
	}

	slices.Sort(inboxes)
	for _, inbox := range slices.Compact(inboxes) {
		// Local inboxes are written directly: the public URL isn't reachable
		// from inside the container.
		if username, ok := strings.CutPrefix(inbox, s.baseURL+"/users/"); ok {
			if !storeLocal {
				continue
			}
			recipient, err := s.store.GetByUsername(ctx, strings.TrimSuffix(username, "/inbox"))
			if err == nil {
				err = s.store.AddInboxNote(ctx, recipient.ID, store.InboxNote{
					ObjectID: note.ID.String(), AttributedTo: a.ActorURI, Content: note.Content.First().String(), Published: note.Published,
				})
			}
			if err != nil {
				log.Printf("deliver to local inbox %s: %v", inbox, err)
			}
			continue
		}
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
