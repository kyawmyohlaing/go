package resourceadapter

import (
	"testing"

	"github.com/guregu/null"
	"github.com/stellar/go/protocols/horizon/effects"
	"github.com/stellar/go/services/horizon/internal/db2/history"
	"github.com/stellar/go/support/test"
	"github.com/stretchr/testify/assert"
)

func TestNewEffect_EffectTrustlineAuthorizedToMaintainLiabilities(t *testing.T) {
	tt := assert.New(t)
	ctx, _ := test.ContextWithLogBuffer()

	details := `{
		"asset_code":   "COP",
		"asset_issuer": "GDRW375MAYR46ODGF2WGANQC2RRZL7O246DYHHCGWTV2RE7IHE2QUQLD",
		"asset_type":   "credit_alphanum4",
		"trustor":      "GDQNY3PBOJOKYZSRMK2S7LHHGWZIUISD4QORETLMXEWXBI7KFZZMKTL3"
	}`

	hEffect := history.Effect{
		Account:            "GDQNY3PBOJOKYZSRMK2S7LHHGWZIUISD4QORETLMXEWXBI7KFZZMKTL3",
		HistoryOperationID: 1,
		Order:              1,
		Type:               history.EffectTrustlineAuthorizedToMaintainLiabilities,
		DetailsString:      null.StringFrom(details),
	}
	resource, err := NewEffect(ctx, hEffect, history.Ledger{})
	tt.NoError(err)

	effect, ok := resource.(effects.TrustlineAuthorizedToMaintainLiabilities)
	tt.True(ok)
	tt.Equal("trustline_authorized_to_maintain_liabilities", effect.Type)
}