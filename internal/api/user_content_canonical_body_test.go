package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// seedSectionEntity writes a vault file (regular `<kind>/<slug>.md`
// layout) + a mirrored DB row so the section endpoints can resolve it.
// Returns the normalized CleanContent the handler will read back (so
// the test can compute the matching If-Match etag).
func seedSectionEntity(t *testing.T, root string, st store.Store, e *vault.Entity) string {
	t.Helper()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(e))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   e.ID,
		Kind: e.Kind,
		Data: e.Data,
	}))
	// Read back so the test sees the same normalized body the handler
	// will (trailing-newline normalization on the round-trip).
	return readVaultByID(t, root, e.Kind, e.ID).CleanContent
}

// TestCanonicalBody_SectionAdd_FirstWriteClaimsOwnership pins ADR-0031
// §5: a canonical thin-edge stamped ugc:true but with no stored
// operator is section-writable by the first operator, who claims
// ownership (their operator is stamped), and a different operator is
// then locked out with 403.
func TestCanonicalBody_SectionAdd_FirstWriteClaimsOwnership(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)

	body := seedSectionEntity(t, root, st, &vault.Entity{
		ID:           "boardgame:moon-colony-bloodbath",
		Kind:         "boardgame",
		Source:       []string{"canonical-label/default"},
		Data:         map[string]any{}, // no operator yet
		CleanContent: "## Overview\nA brutal little game.\n",
		UGC:          true,
	})

	alice := mintToken(t, signer, "alice-agent", "alice")
	rec := ugcReq(t, h, http.MethodPost,
		"/v1/user-content/boardgame:moon-colony-bloodbath/sections", alice,
		map[string]any{"heading": "My take", "body": "Best worst game.\n"},
		map[string]string{"If-Match": userContentEtag(body)},
	)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// Ownership claimed: operator stamped on the vault file + DB row.
	v := readVaultByID(t, root, "boardgame", "boardgame:moon-colony-bloodbath")
	assert.Equal(t, "alice", v.Data["operator"], "first writer claims ownership")
	assert.Equal(t, "alice-agent", v.Data["author"])
	assert.True(t, v.UGC, "ugc flag preserved through the section write")
	assert.Contains(t, v.CleanContent, "My take")

	dbe, err := st.GetEntity(context.Background(), "boardgame:moon-colony-bloodbath")
	require.NoError(t, err)
	assert.Equal(t, "boardgame", dbe.Kind, "DB mirror keeps the canonical kind, not user-content")
	assert.Equal(t, "alice", dbe.Data["operator"])

	// A different operator is now locked out (plain operator-equality).
	bob := mintToken(t, signer, "bob-agent", "bob")
	rec = ugcReq(t, h, http.MethodPost,
		"/v1/user-content/boardgame:moon-colony-bloodbath/sections", bob,
		map[string]any{"heading": "Bob's take", "body": "nope\n"},
		map[string]string{"If-Match": userContentEtag(v.CleanContent)},
	)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
}

// TestCanonicalBody_ConcurrentClaim_SingleWinner pins the fix for the
// first-write ownership race: two operators racing the claim on the
// same unowned canonical body (same starting etag) must not both
// succeed. The entity-level write-lock serializes the read-modify-
// write so exactly one claim lands and the loser is rejected (403
// once the owner is stamped, or 409/412 on the lock/etag boundary).
func TestCanonicalBody_ConcurrentClaim_SingleWinner(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)

	body := seedSectionEntity(t, root, st, &vault.Entity{
		ID:           "boardgame:race-target",
		Kind:         "boardgame",
		Source:       []string{"canonical-label/default"},
		Data:         map[string]any{}, // unowned
		CleanContent: "## Overview\nseed.\n",
		UGC:          true,
	})
	etag := userContentEtag(body)

	alice := mintToken(t, signer, "alice-agent", "alice")
	bob := mintToken(t, signer, "bob-agent", "bob")

	type result struct {
		operator string
		code     int
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		outs []result
	)
	fire := func(operator, tok string) {
		defer wg.Done()
		rec := ugcReq(t, h, http.MethodPost,
			"/v1/user-content/boardgame:race-target/sections", tok,
			map[string]any{"heading": operator + " take", "body": "mine\n"},
			map[string]string{"If-Match": etag},
		)
		mu.Lock()
		outs = append(outs, result{operator: operator, code: rec.Code})
		mu.Unlock()
	}
	wg.Add(2)
	go fire("alice", alice)
	go fire("bob", bob)
	wg.Wait()

	winners := 0
	var winner string
	for _, o := range outs {
		if o.code == http.StatusCreated {
			winners++
			winner = o.operator
		} else {
			// The loser must be a clean rejection, not a 500 / silent
			// double-write: 403 (owner now mismatches), 409 (write
			// lock), or 412 (etag advanced under the lock).
			assert.Contains(t, []int{http.StatusForbidden, http.StatusConflict, http.StatusPreconditionFailed},
				o.code, "loser got %d for operator %s", o.code, o.operator)
		}
	}
	require.Equal(t, 1, winners, "exactly one concurrent claim may win: %+v", outs)

	// The persisted owner is the winner — no clobber by the loser.
	v := readVaultByID(t, root, "boardgame", "boardgame:race-target")
	assert.Equal(t, winner, v.Data["operator"], "stored owner must be the claim winner")
}

// TestSectionTools_PluginSourceRejected pins that a plugin source
// entity (no ugc flag, non-user-content kind) is refused by the
// broadened gate with the reworded invalid_argument envelope.
func TestSectionTools_PluginSourceRejected(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)

	seedSectionEntity(t, root, st, &vault.Entity{
		ID:           "wikipedia:susanna-clarke",
		Kind:         "wikipedia-article",
		Source:       []string{"wikipedia/default"},
		CleanContent: "## Bio\nAuthor.\n",
		// UGC: false (plugin never emits it)
	})

	tok := mintToken(t, signer, "alice-agent", "alice")
	rec := ugcReq(t, h, http.MethodGet,
		"/v1/user-content/wikipedia:susanna-clarke/sections", tok, nil, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_argument")
	assert.Contains(t, rec.Body.String(), "does not accept user-content sections")
}

// TestExistingUGC_NoFlag_StillEditable pins the ADR-0031 §2 back-compat
// kind arm: a pre-existing user-content vault file that predates the
// ugc:true flag (kind=user-content, no flag) stays section-editable.
func TestExistingUGC_NoFlag_StillEditable(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)

	body := seedSectionEntity(t, root, st, &vault.Entity{
		ID:           "user-content:old-note",
		Kind:         "user-content",
		Source:       []string{"user/default"},
		Data:         map[string]any{"operator": "alice"},
		CleanContent: "## A\noriginal\n",
		UGC:          false, // legacy file: implicit-by-kind, no explicit flag
	})

	tok := mintToken(t, signer, "alice-agent", "alice")
	rec := ugcReq(t, h, http.MethodPost,
		"/v1/user-content/user-content:old-note/sections", tok,
		map[string]any{"heading": "B", "body": "added\n"},
		map[string]string{"If-Match": userContentEtag(body)},
	)
	require.Equal(t, http.StatusCreated, rec.Code,
		"pre-flag UGC must stay editable via the kind arm; body=%s", rec.Body.String())
	v := readVaultByID(t, root, "user-content", "user-content:old-note")
	assert.Contains(t, v.CleanContent, "added")
}

// TestCanEditSectionBody is the unit table for the ADR-0031 §5 auth
// branch: pure-UGC empty-operator stays rejected (legacy protection);
// a canonical body with no owner is claimable; ownership-equality
// holds once stamped.
func TestCanEditSectionBody(t *testing.T) {
	t.Parallel()
	alice := &auth.Claim{Subject: "alice-agent", Operator: "alice"}

	cases := []struct {
		name           string
		claim          *auth.Claim
		ve             *vault.Entity
		wantAllowed    bool
		wantClaimOwner bool
	}{
		{
			name:        "UGC legacy row, empty operator -> rejected",
			claim:       alice,
			ve:          &vault.Entity{Kind: "user-content", Data: map[string]any{}},
			wantAllowed: false,
		},
		{
			name:        "UGC matching operator -> allowed, no claim",
			claim:       alice,
			ve:          &vault.Entity{Kind: "user-content", Data: map[string]any{"operator": "alice"}},
			wantAllowed: true,
		},
		{
			name:        "UGC mismatched operator -> rejected",
			claim:       alice,
			ve:          &vault.Entity{Kind: "user-content", Data: map[string]any{"operator": "bob"}},
			wantAllowed: false,
		},
		{
			name:           "canonical body, unowned -> allowed + claim",
			claim:          alice,
			ve:             &vault.Entity{Kind: "boardgame", Data: map[string]any{}},
			wantAllowed:    true,
			wantClaimOwner: true,
		},
		{
			name:        "canonical body, owned by other -> rejected",
			claim:       alice,
			ve:          &vault.Entity{Kind: "boardgame", Data: map[string]any{"operator": "bob"}},
			wantAllowed: false,
		},
		{
			name:        "canonical body, owned by caller -> allowed, no claim",
			claim:       alice,
			ve:          &vault.Entity{Kind: "boardgame", Data: map[string]any{"operator": "alice"}},
			wantAllowed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, claimOwner := canEditSectionBody(tc.claim, tc.ve)
			assert.Equal(t, tc.wantAllowed, allowed, "allowed")
			assert.Equal(t, tc.wantClaimOwner, claimOwner, "claimOwnership")
		})
	}
}
