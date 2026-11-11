package offer

import seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"

// IsRestricted reports whether o is an online maker's quote for a restricted
// asset (RestrictedTerms): settled on the chain under the asset's policy
// server, through the courier, never crossed by the matcher.
func IsRestricted(o *seqobv1.Offer) bool { return o.GetRestricted() != nil }

// RestrictedForBTC reports whether a restricted quote trades the asset for
// on-chain bitcoin (its quote is the BTC sentinel): a swap HTLC on Sequentia
// against a Bitcoin HTLC, which spans a Bitcoin confirmation.
func RestrictedForBTC(o *seqobv1.Offer) bool {
	return IsRestricted(o) && IsBTCSentinel(o.GetPair().GetQuoteAsset())
}
