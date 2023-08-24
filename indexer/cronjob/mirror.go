package cronjob

import (
	"flare-indexer/database"
	"flare-indexer/indexer/config"
	"flare-indexer/indexer/context"
	"flare-indexer/logger"
	"flare-indexer/utils/contracts/mirroring"
	"flare-indexer/utils/merkle"
	"math/big"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

type mirrorCronJob struct {
	db                 *gorm.DB
	epochPeriodSeconds int
	epochTimeSeconds   int64
	mirroringContract  *mirroring.Mirroring
	txOpts             *bind.TransactOpts
}

func NewMirrorCronjob(ctx context.IndexerContext) (Cronjob, error) {
	cfg := ctx.Config()
	mirroringContract, err := newMirroringContract(cfg)
	if err != nil {
		return nil, err
	}

	txOpts, err := TransactOptsFromPrivateKey(cfg.Mirror.PrivateKey, cfg.Chain.ChainID)
	if err != nil {
		return nil, err
	}

	return &mirrorCronJob{
		db:                 ctx.DB(),
		epochPeriodSeconds: int(cfg.Mirror.EpochPeriod / time.Second),
		epochTimeSeconds:   cfg.Mirror.EpochTime.Unix(),
		mirroringContract:  mirroringContract,
		txOpts:             txOpts,
	}, nil
}

func newMirroringContract(cfg *config.Config) (*mirroring.Mirroring, error) {
	eth, err := ethclient.Dial(cfg.Chain.EthRPCURL)
	if err != nil {
		return nil, err
	}

	return mirroring.NewMirroring(cfg.Mirror.MirroringContract, eth)
}

func (c *mirrorCronJob) Name() string {
	return "mirror"
}

func (c *mirrorCronJob) Enabled() bool {
	return false
}

func (c *mirrorCronJob) TimeoutSeconds() int {
	return c.epochPeriodSeconds
}

func (c *mirrorCronJob) Call() error {
	epoch := c.getPreviousEpoch()
	if epoch < 0 {
		return errors.New("invalid epoch")
	}

	txs, err := c.getUnmirroredTxs(epoch)
	if err != nil {
		return err
	}

	if len(txs) == 0 {
		logger.Debug("no unmirrored txs found")
		return nil
	}

	logger.Debug("mirroring %d txs", len(txs))
	if err := c.mirrorTxs(txs, epoch); err != nil {
		return err
	}

	if err := database.MarkTxsAsMirrored(c.db, txs); err != nil {
		return err
	}

	logger.Debug("successfully mirrored %d txs", len(txs))
	return nil
}

func (c *mirrorCronJob) getPreviousEpoch() int64 {
	currEpoch := (time.Now().Unix() - c.epochTimeSeconds) / int64(c.epochPeriodSeconds)
	return currEpoch - 1
}

func (c *mirrorCronJob) getUnmirroredTxs(epoch int64) ([]database.PChainVotingData, error) {
	startTimestamp := time.Unix(c.epochTimeSeconds+(epoch*int64(c.epochPeriodSeconds)), 0)
	endTimestamp := startTimestamp.Add(time.Duration(c.epochPeriodSeconds) * time.Second)

	return database.GetUnmirroredPChainTxs(&database.GetUnmirroredPChainTxsInput{
		DB:             c.db,
		StartTimestamp: startTimestamp,
		EndTimestamp:   endTimestamp,
	})
}

func (c *mirrorCronJob) mirrorTxs(txs []database.PChainVotingData, epochID int64) error {
	merkleTree, err := buildTree(txs)
	if err != nil {
		return err
	}

	for i := range txs {
		in := mirrorTxInput{
			epochID:    big.NewInt(epochID),
			merkleTree: merkleTree,
			tx:         &txs[i],
		}

		if err := c.mirrorTx(&in); err != nil {
			return err
		}
	}

	return nil
}

func buildTree(txs []database.PChainVotingData) (merkle.Tree, error) {
	hashes := make([]common.Hash, len(txs))

	for i := range txs {
		tx := &txs[i]

		if tx.TxID == nil {
			return merkle.Tree{}, errors.New("tx.TxID is nil")
		}

		txHash, err := ids.FromString(*tx.TxID)
		if err != nil {
			return merkle.Tree{}, errors.Wrap(err, "ids.FromString")
		}

		hashes[i] = common.Hash(txHash)
	}

	return merkle.Build(hashes, false), nil
}

type mirrorTxInput struct {
	epochID    *big.Int
	merkleTree merkle.Tree
	tx         *database.PChainVotingData
}

func (c *mirrorCronJob) mirrorTx(in *mirrorTxInput) error {
	txHash, err := ids.FromString(*in.tx.TxID)
	if err != nil {
		return errors.Wrap(err, "ids.FromString")
	}

	stakeData, err := toStakeData(in.tx, in.epochID, txHash)
	if err != nil {
		return err
	}

	merkleProof, err := getMerkleProof(in.merkleTree, txHash)
	if err != nil {
		return err
	}

	_, err = c.mirroringContract.VerifyStake(c.txOpts, *stakeData, merkleProof)
	if err != nil {
		return errors.Wrap(err, "mirroringContract.VerifyStake")
	}

	return nil
}

func toStakeData(
	tx *database.PChainVotingData, epochID *big.Int, txHash [32]byte,
) (*mirroring.IIPChainStakeMirrorVerifierPChainStake, error) {
	txType, err := getTxType(tx.Type)
	if err != nil {
		return nil, err
	}

	nodeID, err := ids.NodeIDFromString(tx.NodeID)
	if err != nil {
		return nil, errors.Wrap(err, "ids.NodeIDFromString")
	}

	if tx.StartTime == nil {
		return nil, errors.New("tx.StartTime is nil")
	}

	startTime := uint64(tx.StartTime.Unix())

	if tx.EndTime == nil {
		return nil, errors.New("tx.EndTime is nil")
	}

	endTime := uint64(tx.EndTime.Unix())

	return &mirroring.IIPChainStakeMirrorVerifierPChainStake{
		EpochId:         epochID,
		BlockNumber:     tx.BlockHeight,
		TransactionHash: txHash,
		TransactionType: txType,
		NodeId:          nodeID,
		StartTime:       startTime,
		EndTime:         endTime,
		Weight:          tx.Weight,
		SourceAddress:   [20]byte(common.HexToAddress(tx.Address)),
		FeePercentage:   uint64(tx.FeePercentage),
	}, nil
}

func getTxType(txType database.PChainTxType) (uint8, error) {
	switch txType {
	case database.PChainAddValidatorTx:
		return 0, nil

	case database.PChainAddDelegatorTx:
		return 1, nil

	default:
		return 0, errors.New("invalid tx type")
	}
}

func getMerkleProof(merkleTree merkle.Tree, txHash [32]byte) ([][32]byte, error) {
	proof, err := merkleTree.GetProofFromHash(txHash)
	if err != nil {
		return nil, errors.Wrap(err, "merkleTree.GetProof")
	}

	proofBytes := make([][32]byte, len(proof))
	for i := range proof {
		proofBytes[i] = [32]byte(proof[i])
	}

	return proofBytes, nil
}
