// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"codeberg.org/gruf/go-kv"
	"github.com/superseriousbusiness/activity/pub"
	"github.com/superseriousbusiness/activity/streams"
	"github.com/superseriousbusiness/activity/streams/vocab"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/log"
)

// IsASMediaType will return whether the given content-type string
// matches one of the 2 possible ActivityStreams incoming content types:
// - application/activity+json
// - application/ld+json;profile=https://w3.org/ns/activitystreams
//
// Where for the above we are leniant with whitespace and quotes.
func IsASMediaType(ct string) bool {
	var (
		// First content-type part,
		// contains the application/...
		p1 string = ct //nolint:revive

		// Second content-type part,
		// contains AS IRI if provided
		p2 string
	)

	// Split content-type by semi-colon.
	sep := strings.IndexByte(ct, ';')
	if sep >= 0 {
		p1 = ct[:sep]
		p2 = ct[sep+1:]
	}

	// Trim any ending space from the
	// main content-type part of string.
	p1 = strings.TrimRight(p1, " ")

	switch p1 {
	case "application/activity+json":
		return p2 == ""

	case "application/ld+json":
		// Trim all start/end space.
		p2 = strings.Trim(p2, " ")

		// Drop any quotes around the URI str.
		p2 = strings.ReplaceAll(p2, "\"", "")

		// End part must be a ref to the main AS namespace IRI.
		return p2 == "profile=https://www.w3.org/ns/activitystreams"

	default:
		return false
	}
}

// federatingActor wraps the pub.FederatingActor
// with some custom GoToSocial-specific logic.
type federatingActor struct {
	sideEffectActor pub.DelegateActor
	wrapped         pub.FederatingActor
}

// newFederatingActor returns a federatingActor.
func newFederatingActor(c pub.CommonBehavior, s2s pub.FederatingProtocol, db pub.Database, clock pub.Clock) pub.FederatingActor {
	sideEffectActor := pub.NewSideEffectActor(c, s2s, nil, db, clock)
	sideEffectActor.Serialize = ap.Serialize // hook in our own custom Serialize function

	return &federatingActor{
		sideEffectActor: sideEffectActor,
		wrapped:         pub.NewCustomActor(sideEffectActor, false, true, clock),
	}
}

// PostInboxScheme is a reimplementation of the default baseActor
// implementation of PostInboxScheme in pub/base_actor.go.
//
// Key differences from that implementation:
//   - More explicit debug logging when a request is not processed.
//   - Normalize content of activity object.
//   - *ALWAYS* return gtserror.WithCode if there's an issue, to
//     provide more helpful messages to remote callers.
//   - Return code 202 instead of 200 on successful POST, to reflect
//     that we process most side effects asynchronously.
func (f *federatingActor) PostInboxScheme(ctx context.Context, w http.ResponseWriter, r *http.Request, scheme string) (bool, error) {
	l := log.WithContext(ctx).
		WithFields([]kv.Field{
			{"userAgent", r.UserAgent()},
			{"path", r.URL.Path},
		}...)

	// Ensure valid ActivityPub Content-Type.
	// https://www.w3.org/TR/activitypub/#server-to-server-interactions
	if ct := r.Header.Get("Content-Type"); !IsASMediaType(ct) {
		const ct1 = "application/activity+json"
		const ct2 = "application/ld+json;profile=https://w3.org/ns/activitystreams"
		err := fmt.Errorf("Content-Type %s not acceptable, this endpoint accepts: [%q %q]", ct, ct1, ct2)
		return false, gtserror.NewErrorNotAcceptable(err)
	}

	// Authenticate request by checking http signature.
	ctx, authenticated, err := f.sideEffectActor.AuthenticatePostInbox(ctx, w, r)
	if err != nil {
		return false, gtserror.NewErrorInternalError(err)
	}

	if !authenticated {
		err = errors.New("not authenticated")
		return false, gtserror.NewErrorUnauthorized(err)
	}

	/*
		Begin processing the request, but note that we
		have not yet applied authorization (ie., blocks).
	*/

	// Obtain the activity; reject unknown activities.
	activity, errWithCode := resolveActivity(ctx, r)
	if errWithCode != nil {
		return false, errWithCode
	}

	// Set additional context data. Primarily this means
	// looking at the Activity and seeing which IRIs are
	// involved in it tangentially.
	ctx, err = f.sideEffectActor.PostInboxRequestBodyHook(ctx, r, activity)
	if err != nil {
		return false, gtserror.NewErrorInternalError(err)
	}

	// Check authorization of the activity; this will include blocks.
	authorized, err := f.sideEffectActor.AuthorizePostInbox(ctx, w, activity)
	if err != nil {
		if errors.As(err, new(errOtherIRIBlocked)) {
			// There's no direct block between requester(s) and
			// receiver. However, one or more of the other IRIs
			// involved in the request (account replied to, note
			// boosted, etc) is blocked either at domain level or
			// by the receiver. We don't need to return 403 here,
			// instead, just return 202 accepted but don't do any
			// further processing of the activity.
			return true, nil
		}

		// Real error has occurred.
		return false, gtserror.NewErrorInternalError(err)
	}

	if !authorized {
		// Block exists either from this instance against
		// one or more directly involved actors, or between
		// receiving account and one of those actors.
		err = errors.New("blocked")
		return false, gtserror.NewErrorForbidden(err)
	}

	// Copy existing URL + add request host and scheme.
	inboxID := func() *url.URL {
		u := new(url.URL)
		*u = *r.URL
		u.Host = r.Host
		u.Scheme = scheme
		return u
	}()

	// At this point we have everything we need, and have verified that
	// the POST request is authentic (properly signed) and authorized
	// (permitted to interact with the target inbox).
	//
	// Post the activity to the Actor's inbox and trigger side effects .
	if err := f.sideEffectActor.PostInbox(ctx, inboxID, activity); err != nil {
		// Special case: We know it is a bad request if the object or
		// target properties needed to be populated, but weren't.
		// Send the rejection to the peer.
		if errors.Is(err, pub.ErrObjectRequired) || errors.Is(err, pub.ErrTargetRequired) {
			// Log the original error but return something a bit more generic.
			l.Debugf("malformed incoming Activity: %q", err)
			err = errors.New("malformed incoming Activity: an Object and/or Target was required but not set")
			return false, gtserror.NewErrorBadRequest(err, err.Error())
		}

		// There's been some real error.
		err = fmt.Errorf("PostInboxScheme: error calling sideEffectActor.PostInbox: %w", err)
		return false, gtserror.NewErrorInternalError(err)
	}

	// Side effects are complete. Now delegate determining whether
	// to do inbox forwarding, as well as the action to do it.
	if err := f.sideEffectActor.InboxForwarding(ctx, inboxID, activity); err != nil {
		// As a not-ideal side-effect, InboxForwarding will try
		// to create entries if the federatingDB returns `false`
		// when calling `Exists()` to determine whether the Activity
		// is in the database.
		//
		// Since our `Exists()` function currently *always*
		// returns false, it will *always* attempt to insert
		// the Activity. Therefore, we ignore AlreadyExists
		// errors.
		//
		// This check may be removed when the `Exists()` func
		// is updated, and/or federating callbacks are handled
		// properly.
		if !errors.Is(err, db.ErrAlreadyExists) {
			// Failed inbox forwarding is not a show-stopper,
			// and doesn't even necessarily denote a real error.
			l.Warnf("error calling sideEffectActor.InboxForwarding: %q", err)
		}
	}

	// Request is now undergoing processing. Caller
	// of this function will handle writing Accepted.
	return true, nil
}

// resolveActivity is a util function for pulling a
// pub.Activity type out of an incoming POST request.
func resolveActivity(ctx context.Context, r *http.Request) (pub.Activity, gtserror.WithCode) {
	// Tidy up when done.
	defer r.Body.Close()

	b, err := io.ReadAll(r.Body)
	if err != nil {
		err = fmt.Errorf("error reading request body: %w", err)
		return nil, gtserror.NewErrorInternalError(err)
	}

	var rawActivity map[string]interface{}
	if err := json.Unmarshal(b, &rawActivity); err != nil {
		err = fmt.Errorf("error unmarshalling request body: %w", err)
		return nil, gtserror.NewErrorInternalError(err)
	}

	t, err := streams.ToType(ctx, rawActivity)
	if err != nil {
		if !streams.IsUnmatchedErr(err) {
			// Real error.
			err = fmt.Errorf("error matching json to type: %w", err)
			return nil, gtserror.NewErrorInternalError(err)
		}

		// Respond with bad request; we just couldn't
		// match the type to one that we know about.
		err = errors.New("body json could not be resolved to ActivityStreams value")
		return nil, gtserror.NewErrorBadRequest(err, err.Error())
	}

	activity, ok := t.(pub.Activity)
	if !ok {
		err = fmt.Errorf("ActivityStreams value with type %T is not a pub.Activity", t)
		return nil, gtserror.NewErrorBadRequest(err, err.Error())
	}

	if activity.GetJSONLDId() == nil {
		err = fmt.Errorf("incoming Activity %s did not have required id property set", activity.GetTypeName())
		return nil, gtserror.NewErrorBadRequest(err, err.Error())
	}

	// If activity Object is a Statusable, we'll want to replace the
	// parsed `content` value with the value from the raw JSON instead.
	// See https://github.com/superseriousbusiness/gotosocial/issues/1661
	// Likewise, if it's an Accountable, we'll normalize some fields on it.
	ap.NormalizeIncomingActivityObject(activity, rawActivity)

	return activity, nil
}

/*
	Functions below are just lightly wrapped versions
	of the original go-fed federatingActor functions.
*/

func (f *federatingActor) PostInbox(c context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	return f.PostInboxScheme(c, w, r, "https")
}

func (f *federatingActor) Send(c context.Context, outbox *url.URL, t vocab.Type) (pub.Activity, error) {
	log.Infof(c, "send activity %s via outbox %s", t.GetTypeName(), outbox)
	return f.wrapped.Send(c, outbox, t)
}

func (f *federatingActor) GetInbox(c context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	return f.wrapped.GetInbox(c, w, r)
}

func (f *federatingActor) PostOutbox(c context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	return f.wrapped.PostOutbox(c, w, r)
}

func (f *federatingActor) PostOutboxScheme(c context.Context, w http.ResponseWriter, r *http.Request, scheme string) (bool, error) {
	return f.wrapped.PostOutboxScheme(c, w, r, scheme)
}

func (f *federatingActor) GetOutbox(c context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	return f.wrapped.GetOutbox(c, w, r)
}
