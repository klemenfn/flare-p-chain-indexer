package xchain

import (
	"flare-indexer/database"
	"flare-indexer/indexer/shared"
	"flare-indexer/utils"
	"flare-indexer/utils/chain"
	"fmt"

	"github.com/ava-labs/avalanchego/indexer"
	"github.com/ava-labs/avalanchego/vms/avm/txs"
	"github.com/ava-labs/avalanchego/wallet/chain/x"
	"gorm.io/gorm"
)

type xChainInputUpdater struct {
	shared.BaseInputUpdater

	db     *gorm.DB
	client indexer.Client
}

func newXChainInputUpdater(db *gorm.DB, client indexer.Client) *xChainInputUpdater {
	return &xChainInputUpdater{
		db:     db,
		client: client,
	}
}

func (iu *xChainInputUpdater) CacheOutputs(txID string, outs []*database.TxOutput) {
	iu.BaseInputUpdater.CacheOutputs(txID, outs)
}

func (iu *xChainInputUpdater) UpdateInputs(inputs []*database.TxInput) error {
	notUpdated := iu.BaseInputUpdater.UpdateInputsFromCache(inputs)
	err := iu.updateFromDB(notUpdated)
	if err != nil {
		return err
	}
	err = iu.updateFromChain(notUpdated)
	if err != nil {
		return err
	}
	if len(notUpdated) > 0 {
		return fmt.Errorf("unable to fetch transactions with ids %v", utils.Keys(notUpdated))
	}
	return nil
}

// notUpdated is a map from *output* id to inputs referring this output
func (iu *xChainInputUpdater) updateFromDB(notUpdated map[string][]*database.TxInput) error {
	outs, err := database.FetchXChainTxOutputs(iu.db, utils.Keys(notUpdated))
	if err != nil {
		return err
	}
	baseOuts := make([]*database.TxOutput, len(outs))
	for i, o := range outs {
		baseOuts[i] = &o.TxOutput
	}
	return shared.UpdateInputsWithOutputs(notUpdated, baseOuts)
}

// notUpdated is a map from *output* id to inputs referring this output
func (iu *xChainInputUpdater) updateFromChain(notUpdated map[string][]*database.TxInput) error {
	fetchedOuts := make([]*database.TxOutput, 0, 4*len(notUpdated))
	for txId := range notUpdated {
		container, err := chain.FetchContainerFromIndexer(iu.client, txId)
		if err != nil {
			return err
		}
		if container == nil {
			continue
		}

		tx, err := x.Parser.ParseGenesisTx(container.Bytes)
		if err != nil {
			return err
		}

		var outs []*database.TxOutput
		switch unsignedTx := tx.Unsigned.(type) {
		case *txs.BaseTx:
			outs, err = shared.TxOutputsFromBaseTx(txId, unsignedTx)
		case *txs.ImportTx:
			outs, err = shared.TxOutputsFromBaseTx(txId, &unsignedTx.BaseTx)
		default:
			return fmt.Errorf("transaction with id %s has unsupported type %T", container.ID.String(), unsignedTx)
		}
		if err != nil {
			return err
		}

		fetchedOuts = append(fetchedOuts, outs...)
	}
	return shared.UpdateInputsWithOutputs(notUpdated, fetchedOuts)
}
