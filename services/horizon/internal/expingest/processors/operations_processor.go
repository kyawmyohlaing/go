package processors

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/stellar/go/amount"
	"github.com/stellar/go/exp/ingest/io"
	"github.com/stellar/go/services/horizon/internal/db2/history"
	"github.com/stellar/go/services/horizon/internal/toid"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/xdr"
)

// OperationProcessor operations processor
type OperationProcessor struct {
	operationsQ history.QOperations

	sequence uint32
	batch    history.OperationBatchInsertBuilder
}

func NewOperationProcessor(operationsQ history.QOperations, sequence uint32) *OperationProcessor {
	return &OperationProcessor{
		operationsQ: operationsQ,
		sequence:    sequence,
		batch:       operationsQ.NewOperationBatchInsertBuilder(maxBatchSize),
	}
}

// ProcessTransaction process the given transaction
func (p *OperationProcessor) ProcessTransaction(transaction io.LedgerTransaction) (err error) {
	for i, op := range transaction.Envelope.Operations() {
		operation := transactionOperationWrapper{
			index:          uint32(i),
			transaction:    transaction,
			operation:      op,
			ledgerSequence: p.sequence,
		}
		details, err := operation.Details()
		if err != nil {
			return errors.Wrapf(err, "Error obtaining details for operation %v", operation.ID())
		}
		var detailsJSON []byte
		detailsJSON, err = json.Marshal(details)
		if err != nil {
			return errors.Wrapf(err, "Error marshaling details for operation %v", operation.ID())
		}

		if err = p.batch.Add(
			operation.ID(),
			operation.TransactionID(),
			operation.Order(),
			operation.OperationType(),
			detailsJSON,
			operation.SourceAccount().Address(),
		); err != nil {
			return errors.Wrap(err, "Error batch inserting operation rows")
		}
	}

	return err
}

func (p *OperationProcessor) Commit() error {
	return p.batch.Exec()
}

// transactionOperationWrapper represents the data for a single operation within a transaction
type transactionOperationWrapper struct {
	index          uint32
	transaction    io.LedgerTransaction
	operation      xdr.Operation
	ledgerSequence uint32
}

// ID returns the ID for the operation.
func (operation *transactionOperationWrapper) ID() int64 {
	return toid.New(
		int32(operation.ledgerSequence),
		int32(operation.transaction.Index),
		int32(operation.index+1),
	).ToInt64()
}

// Order returns the operation order.
func (operation *transactionOperationWrapper) Order() uint32 {
	return operation.index + 1
}

// TransactionID returns the id for the transaction related with this operation.
func (operation *transactionOperationWrapper) TransactionID() int64 {
	return toid.New(int32(operation.ledgerSequence), int32(operation.transaction.Index), 0).ToInt64()
}

// SourceAccount returns the operation's source account.
func (operation *transactionOperationWrapper) SourceAccount() *xdr.AccountId {
	sourceAccount := operation.operation.SourceAccount
	var sa xdr.AccountId
	if sourceAccount != nil {
		sa = sourceAccount.ToAccountId()
	} else {
		sa = operation.transaction.Envelope.SourceAccount().ToAccountId()
	}

	return &sa
}

// OperationType returns the operation type.
func (operation *transactionOperationWrapper) OperationType() xdr.OperationType {
	return operation.operation.Body.Type
}

func (operation *transactionOperationWrapper) getSponsor() (*xdr.AccountId, error) {
	changes, err := operation.transaction.GetOperationChanges(operation.index)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.Pre != nil || c.Post == nil {
			// We are looking for entry creations
			continue
		}
		if sponsorAccount := c.Post.SponsoringID(); sponsorAccount != nil {
			return sponsorAccount, nil
		}
	}
	return nil, nil
}

// OperationResult returns the operation's result record
func (operation *transactionOperationWrapper) OperationResult() *xdr.OperationResultTr {
	results, _ := operation.transaction.Result.OperationResults()
	tr := results[operation.index].MustTr()
	return &tr
}

func (operation *transactionOperationWrapper) findPriorBeginSponsoringOp() *xdr.Operation {
	operations := operation.transaction.Envelope.Operations()
	for i := int(operation.index) - 1; i >= 0; i-- {
		if operations[i].Body.Type == xdr.OperationTypeBeginSponsoringFutureReserves {
			return &operations[i]
		}
	}
	return nil
}

// Details returns the operation details as a map which can be stored as JSON.
func (operation *transactionOperationWrapper) Details() (map[string]interface{}, error) {
	details := map[string]interface{}{}
	source := operation.SourceAccount()
	switch operation.OperationType() {
	case xdr.OperationTypeCreateAccount:
		op := operation.operation.Body.MustCreateAccountOp()
		details["funder"] = source.Address()
		details["account"] = op.Destination.Address()
		details["starting_balance"] = amount.String(op.StartingBalance)
	case xdr.OperationTypePayment:
		op := operation.operation.Body.MustPaymentOp()
		details["from"] = source.Address()
		accid := op.Destination.ToAccountId()
		details["to"] = accid.Address()
		details["amount"] = amount.String(op.Amount)
		assetDetails(details, op.Asset, "")
	case xdr.OperationTypePathPaymentStrictReceive:
		op := operation.operation.Body.MustPathPaymentStrictReceiveOp()
		details["from"] = source.Address()
		accid := op.Destination.ToAccountId()
		details["to"] = accid.Address()

		details["amount"] = amount.String(op.DestAmount)
		details["source_amount"] = amount.String(0)
		details["source_max"] = amount.String(op.SendMax)
		assetDetails(details, op.DestAsset, "")
		assetDetails(details, op.SendAsset, "source_")

		if operation.transaction.Result.Successful() {
			result := operation.OperationResult().MustPathPaymentStrictReceiveResult()
			details["source_amount"] = amount.String(result.SendAmount())
		}

		var path = make([]map[string]interface{}, len(op.Path))
		for i := range op.Path {
			path[i] = make(map[string]interface{})
			assetDetails(path[i], op.Path[i], "")
		}
		details["path"] = path

	case xdr.OperationTypePathPaymentStrictSend:
		op := operation.operation.Body.MustPathPaymentStrictSendOp()
		details["from"] = source.Address()
		accid := op.Destination.ToAccountId()
		details["to"] = accid.Address()

		details["amount"] = amount.String(0)
		details["source_amount"] = amount.String(op.SendAmount)
		details["destination_min"] = amount.String(op.DestMin)
		assetDetails(details, op.DestAsset, "")
		assetDetails(details, op.SendAsset, "source_")

		if operation.transaction.Result.Successful() {
			result := operation.OperationResult().MustPathPaymentStrictSendResult()
			details["amount"] = amount.String(result.DestAmount())
		}

		var path = make([]map[string]interface{}, len(op.Path))
		for i := range op.Path {
			path[i] = make(map[string]interface{})
			assetDetails(path[i], op.Path[i], "")
		}
		details["path"] = path
	case xdr.OperationTypeManageBuyOffer:
		op := operation.operation.Body.MustManageBuyOfferOp()
		details["offer_id"] = op.OfferId
		details["amount"] = amount.String(op.BuyAmount)
		details["price"] = op.Price.String()
		details["price_r"] = map[string]interface{}{
			"n": op.Price.N,
			"d": op.Price.D,
		}
		assetDetails(details, op.Buying, "buying_")
		assetDetails(details, op.Selling, "selling_")
	case xdr.OperationTypeManageSellOffer:
		op := operation.operation.Body.MustManageSellOfferOp()
		details["offer_id"] = op.OfferId
		details["amount"] = amount.String(op.Amount)
		details["price"] = op.Price.String()
		details["price_r"] = map[string]interface{}{
			"n": op.Price.N,
			"d": op.Price.D,
		}
		assetDetails(details, op.Buying, "buying_")
		assetDetails(details, op.Selling, "selling_")
	case xdr.OperationTypeCreatePassiveSellOffer:
		op := operation.operation.Body.MustCreatePassiveSellOfferOp()
		details["amount"] = amount.String(op.Amount)
		details["price"] = op.Price.String()
		details["price_r"] = map[string]interface{}{
			"n": op.Price.N,
			"d": op.Price.D,
		}
		assetDetails(details, op.Buying, "buying_")
		assetDetails(details, op.Selling, "selling_")
	case xdr.OperationTypeSetOptions:
		op := operation.operation.Body.MustSetOptionsOp()

		if op.InflationDest != nil {
			details["inflation_dest"] = op.InflationDest.Address()
		}

		if op.SetFlags != nil && *op.SetFlags > 0 {
			operationFlagDetails(details, int32(*op.SetFlags), "set")
		}

		if op.ClearFlags != nil && *op.ClearFlags > 0 {
			operationFlagDetails(details, int32(*op.ClearFlags), "clear")
		}

		if op.MasterWeight != nil {
			details["master_key_weight"] = *op.MasterWeight
		}

		if op.LowThreshold != nil {
			details["low_threshold"] = *op.LowThreshold
		}

		if op.MedThreshold != nil {
			details["med_threshold"] = *op.MedThreshold
		}

		if op.HighThreshold != nil {
			details["high_threshold"] = *op.HighThreshold
		}

		if op.HomeDomain != nil {
			details["home_domain"] = *op.HomeDomain
		}

		if op.Signer != nil {
			details["signer_key"] = op.Signer.Key.Address()
			details["signer_weight"] = op.Signer.Weight
		}
	case xdr.OperationTypeChangeTrust:
		op := operation.operation.Body.MustChangeTrustOp()
		assetDetails(details, op.Line, "")
		details["trustor"] = source.Address()
		details["trustee"] = details["asset_issuer"]
		details["limit"] = amount.String(op.Limit)
	case xdr.OperationTypeAllowTrust:
		op := operation.operation.Body.MustAllowTrustOp()
		assetDetails(details, op.Asset.ToAsset(*source), "")
		details["trustee"] = source.Address()
		details["trustor"] = op.Trustor.Address()
		details["authorize"] = xdr.TrustLineFlags(op.Authorize).IsAuthorized()
		if xdr.TrustLineFlags(op.Authorize).IsAuthorizedToMaintainLiabilitiesFlag() {
			details["authorize_to_maintain_liabilities"] = xdr.TrustLineFlags(op.Authorize).IsAuthorizedToMaintainLiabilitiesFlag()
		}
	case xdr.OperationTypeAccountMerge:
		aid := operation.operation.Body.MustDestination().ToAccountId()
		details["account"] = source.Address()
		details["into"] = aid.Address()
	case xdr.OperationTypeInflation:
		// no inflation details, presently
	case xdr.OperationTypeManageData:
		op := operation.operation.Body.MustManageDataOp()
		details["name"] = string(op.DataName)
		if op.DataValue != nil {
			details["value"] = base64.StdEncoding.EncodeToString(*op.DataValue)
		} else {
			details["value"] = nil
		}
	case xdr.OperationTypeBumpSequence:
		op := operation.operation.Body.MustBumpSequenceOp()
		details["bump_to"] = fmt.Sprintf("%d", op.BumpTo)
	case xdr.OperationTypeCreateClaimableBalance:
		op := operation.operation.Body.MustCreateClaimableBalanceOp()
		details["asset"] = op.Asset.StringCanonical()
		details["amount"] = amount.String(op.Amount)
		var claimants history.Claimants
		for _, c := range op.Claimants {
			cv0 := c.MustV0()
			claimants = append(claimants, history.Claimant{
				Destination: cv0.Destination.Address(),
				Predicate:   cv0.Predicate,
			})
		}
		details["claimants"] = claimants
	case xdr.OperationTypeClaimClaimableBalance:
		op := operation.operation.Body.MustClaimClaimableBalanceOp()
		balanceID, err := xdr.MarshalBase64(op.BalanceId)
		if err != nil {
			panic(fmt.Errorf("Invalid balanceId in op: %d", operation.index))
		}
		details["balance_id"] = balanceID
		details["claimant"] = source.Address()
	case xdr.OperationTypeBeginSponsoringFutureReserves:
		op := operation.operation.Body.MustBeginSponsoringFutureReservesOp()
		details["sponsored_id"] = op.SponsoredId.Address()
	case xdr.OperationTypeEndSponsoringFutureReserves:
		beginSponsoringOp := operation.findPriorBeginSponsoringOp()
		if beginSponsoringOp == nil {
			return nil, fmt.Errorf("prior Begin operation not found for EndSponsoringFutureReserves")
		}
		beginSponsor := beginSponsoringOp.SourceAccount.ToAccountId()
		details["begin_sponsor"] = beginSponsor.Address()
	case xdr.OperationTypeRevokeSponsorship:
		op := operation.operation.Body.MustRevokeSponsorshipOp()
		switch op.Type {
		case xdr.RevokeSponsorshipTypeRevokeSponsorshipLedgerEntry:
			if err := ledgerKeyDetails(details, *op.LedgerKey, ""); err != nil {
				return nil, err
			}
		case xdr.RevokeSponsorshipTypeRevokeSponsorshipSigner:
			details["signer_account"] = op.Signer.AccountId.Address()
			details["signer_key"] = op.Signer.SignerKey.Address()
		}
	default:
		panic(fmt.Errorf("Unknown operation type: %s", operation.OperationType()))
	}

	if sponsor, _ := operation.getSponsor(); sponsor != nil {
		details["sponsor"] = sponsor.Address()
	}

	return details, nil
}

// assetDetails sets the details for `a` on `result` using keys with `prefix`
func assetDetails(result map[string]interface{}, a xdr.Asset, prefix string) error {
	var (
		assetType string
		code      string
		issuer    string
	)
	err := a.Extract(&assetType, &code, &issuer)
	if err != nil {
		err = errors.Wrap(err, "xdr.Asset.Extract error")
		return err
	}
	result[prefix+"asset_type"] = assetType

	if a.Type == xdr.AssetTypeAssetTypeNative {
		return nil
	}

	result[prefix+"asset_code"] = code
	result[prefix+"asset_issuer"] = issuer
	return nil
}

// operationFlagDetails sets the account flag details for `f` on `result`.
func operationFlagDetails(result map[string]interface{}, f int32, prefix string) {
	var (
		n []int32
		s []string
	)

	if (f & int32(xdr.AccountFlagsAuthRequiredFlag)) > 0 {
		n = append(n, int32(xdr.AccountFlagsAuthRequiredFlag))
		s = append(s, "auth_required")
	}

	if (f & int32(xdr.AccountFlagsAuthRevocableFlag)) > 0 {
		n = append(n, int32(xdr.AccountFlagsAuthRevocableFlag))
		s = append(s, "auth_revocable")
	}

	if (f & int32(xdr.AccountFlagsAuthImmutableFlag)) > 0 {
		n = append(n, int32(xdr.AccountFlagsAuthImmutableFlag))
		s = append(s, "auth_immutable")
	}

	result[prefix+"_flags"] = n
	result[prefix+"_flags_s"] = s
}

func ledgerKeyDetails(result map[string]interface{}, ledgerKey xdr.LedgerKey, prefix string) error {
	switch ledgerKey.Type {
	case xdr.LedgerEntryTypeAccount:
		result[prefix+"account_id"] = ledgerKey.Account.AccountId.Address()
	case xdr.LedgerEntryTypeClaimableBalance:
		b64, err := xdr.MarshalBase64(ledgerKey.ClaimableBalance.BalanceId)
		if err != nil {
			return errors.Wrapf(err, "in claimable balance")
		}
		result[prefix+"claimable_balance_id"] = b64
	case xdr.LedgerEntryTypeData:
		result[prefix+"account_id"] = ledgerKey.Data.AccountId.Address()
		result[prefix+"data_name"] = ledgerKey.Data.DataName
	case xdr.LedgerEntryTypeOffer:
		result[prefix+"offer_id"] = ledgerKey.Offer.OfferId
	case xdr.LedgerEntryTypeTrustline:
		result[prefix+"account_id"] = ledgerKey.TrustLine.AccountId.Address()
		assetDetails(result, ledgerKey.TrustLine.Asset, prefix)
	}
	return nil
}

// Participants returns the accounts taking part in the operation.
func (operation *transactionOperationWrapper) Participants() ([]xdr.AccountId, error) {
	participants := []xdr.AccountId{}
	participants = append(participants, *operation.SourceAccount())
	op := operation.operation

	switch operation.OperationType() {
	case xdr.OperationTypeCreateAccount:
		participants = append(participants, op.Body.MustCreateAccountOp().Destination)
	case xdr.OperationTypePayment:
		participants = append(participants, op.Body.MustPaymentOp().Destination.ToAccountId())
	case xdr.OperationTypePathPaymentStrictReceive:
		participants = append(participants, op.Body.MustPathPaymentStrictReceiveOp().Destination.ToAccountId())
	case xdr.OperationTypePathPaymentStrictSend:
		participants = append(participants, op.Body.MustPathPaymentStrictSendOp().Destination.ToAccountId())
	case xdr.OperationTypeManageBuyOffer:
		// the only direct participant is the source_account
	case xdr.OperationTypeManageSellOffer:
		// the only direct participant is the source_account
	case xdr.OperationTypeCreatePassiveSellOffer:
		// the only direct participant is the source_account
	case xdr.OperationTypeSetOptions:
		// the only direct participant is the source_account
	case xdr.OperationTypeChangeTrust:
		// the only direct participant is the source_account
	case xdr.OperationTypeAllowTrust:
		participants = append(participants, op.Body.MustAllowTrustOp().Trustor)
	case xdr.OperationTypeAccountMerge:
		participants = append(participants, op.Body.MustDestination().ToAccountId())
	case xdr.OperationTypeInflation:
		// the only direct participant is the source_account
	case xdr.OperationTypeManageData:
		// the only direct participant is the source_account
	case xdr.OperationTypeBumpSequence:
		// the only direct participant is the source_account
	case xdr.OperationTypeCreateClaimableBalance:
		for _, c := range op.Body.MustCreateClaimableBalanceOp().Claimants {
			participants = append(participants, c.MustV0().Destination)
		}
	case xdr.OperationTypeClaimClaimableBalance:
		// the only direct participant is the source_account
	case xdr.OperationTypeBeginSponsoringFutureReserves:
		participants = append(participants, op.Body.MustBeginSponsoringFutureReservesOp().SponsoredId)
	case xdr.OperationTypeEndSponsoringFutureReserves:
		beginSponsoringOp := operation.findPriorBeginSponsoringOp()
		if beginSponsoringOp == nil {
			return nil, fmt.Errorf("prior Begin operation not found for EndSponsoringFutureReserves")
		}
		beginSponsor := beginSponsoringOp.SourceAccount.ToAccountId()
		participants = append(participants, beginSponsor)
	case xdr.OperationTypeRevokeSponsorship:
		// the only direct participant is the source_account
	default:
		return participants, fmt.Errorf("Unknown operation type: %s", op.Body.Type)
	}

	if sponsor, _ := operation.getSponsor(); sponsor != nil {
		participants = append(participants, *sponsor)
	}

	return dedupe(participants), nil
}

// dedupe remove any duplicate ids from `in`
func dedupe(in []xdr.AccountId) (out []xdr.AccountId) {
	set := map[string]xdr.AccountId{}
	for _, id := range in {
		set[id.Address()] = id
	}

	for _, id := range set {
		out = append(out, id)
	}
	return
}

// OperationsParticipants returns a map with all participants per operation
func operationsParticipants(transaction io.LedgerTransaction, sequence uint32) (map[int64][]xdr.AccountId, error) {
	participants := map[int64][]xdr.AccountId{}

	for opi, op := range transaction.Envelope.Operations() {
		operation := transactionOperationWrapper{
			index:          uint32(opi),
			transaction:    transaction,
			operation:      op,
			ledgerSequence: sequence,
		}

		p, err := operation.Participants()
		if err != nil {
			return participants, errors.Wrapf(err, "reading operation %v participants", operation.ID())
		}
		participants[operation.ID()] = p
	}

	return participants, nil
}
