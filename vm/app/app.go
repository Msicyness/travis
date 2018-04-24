package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/CyberMiles/travis/api"
	emtTypes "github.com/CyberMiles/travis/vm/types"

	"github.com/CyberMiles/travis/errors"
	"github.com/CyberMiles/travis/utils"
	abciTypes "github.com/tendermint/abci/types"
	tmLog "github.com/tendermint/tmlibs/log"
)

const (
	MinGasPrice = 2e9 // 2 Gwei
)

type FromTo struct {
	from common.Address
	to   common.Address
}

// EthermintApplication implements an ABCI application
// #stable - 0.4.0
type EthermintApplication struct {

	// backend handles the ethereum state machine
	// and wrangles other services started by an ethereum node (eg. tx pool)
	backend *api.Backend // backend ethereum struct

	// a closure to return the latest current state from the ethereum blockchain
	getCurrentState func() (*state.StateDB, error)

	checkTxState *state.StateDB

	// an ethereum rpc client we can forward queries to
	rpcClient *rpc.Client

	// strategy for validator compensation
	strategy *emtTypes.Strategy

	logger tmLog.Logger

	lowPriceTransactions map[FromTo]*ethTypes.Transaction

	// record count of failed CheckTx of each from account; used to feed in the nonce check
	checkFailedCount map[common.Address]uint64

	mu           sync.RWMutex
}

// NewEthermintApplication creates a fully initialised instance of EthermintApplication
// #stable - 0.4.0
func NewEthermintApplication(backend *api.Backend,
	client *rpc.Client, strategy *emtTypes.Strategy) (*EthermintApplication, error) {

	state, err := backend.Ethereum().BlockChain().State()
	if err != nil {
		return nil, err
	}

	app := &EthermintApplication{
		backend:              backend,
		rpcClient:            client,
		getCurrentState:      backend.Ethereum().BlockChain().State,
		checkTxState:         state.Copy(),
		strategy:             strategy,
		lowPriceTransactions: make(map[FromTo]*ethTypes.Transaction),
		checkFailedCount:     make(map[common.Address]uint64),
	}

	if err := app.backend.InitEthState(app.Receiver()); err != nil {
		return nil, err
	}

	return app, nil
}

// SetLogger sets the logger for the ethermint application
// #unstable
func (app *EthermintApplication) SetLogger(log tmLog.Logger) {
	app.logger = log
}

var bigZero = big.NewInt(0)

// maxTransactionSize is 32KB in order to prevent DOS attacks
const maxTransactionSize = 32768

// Info returns information about the last height and app_hash to the tendermint engine
// #stable - 0.4.0

func (app *EthermintApplication) Info(req abciTypes.RequestInfo) abciTypes.ResponseInfo {
	blockchain := app.backend.Ethereum().BlockChain()
	currentBlock := blockchain.CurrentBlock()
	height := currentBlock.Number()
	hash := currentBlock.Hash()

	app.logger.Debug("Info", "height", height) // nolint: errcheck

	// This check determines whether it is the first time ethermint gets started.
	// If it is the first time, then we have to respond with an empty hash, since
	// that is what tendermint expects.
	if height.Cmp(bigZero) == 0 {
		return abciTypes.ResponseInfo{
			Data:             "ABCIEthereum",
			LastBlockHeight:  height.Int64(),
			LastBlockAppHash: []byte{},
		}
	}

	return abciTypes.ResponseInfo{
		Data:             "ABCIEthereum",
		LastBlockHeight:  height.Int64(),
		LastBlockAppHash: hash[:],
	}
}

// SetOption sets a configuration option
// #stable - 0.4.0
func (app *EthermintApplication) SetOption(req abciTypes.RequestSetOption) abciTypes.ResponseSetOption {

	app.logger.Debug("SetOption", "key", req.GetKey(), "value", req.GetValue()) // nolint: errcheck
	return abciTypes.ResponseSetOption{}
}

// InitChain initializes the validator set
// #stable - 0.4.0
func (app *EthermintApplication) InitChain(req abciTypes.RequestInitChain) abciTypes.ResponseInitChain {

	app.logger.Debug("InitChain") // nolint: errcheck
	app.SetValidators(req.GetValidators())
	return abciTypes.ResponseInitChain{}
}

// CheckTx checks a transaction is valid but does not mutate the state
// #stable - 0.4.0
func (app *EthermintApplication) CheckTx(tx *ethTypes.Transaction) abciTypes.ResponseCheckTx {
	app.logger.Debug("CheckTx: Received valid transaction", "tx", tx) // nolint: errcheck

	return app.validateTx(tx)
}

// DeliverTx executes a transaction against the latest state
// #stable - 0.4.0
func (app *EthermintApplication) DeliverTx(tx *ethTypes.Transaction) abciTypes.ResponseDeliverTx {
	app.logger.Debug("DeliverTx: Received valid transaction", "tx", tx) // nolint: errcheck

	res := app.backend.DeliverTx(tx)
	if res.IsErr() {
		// nolint: errcheck
		app.logger.Error("DeliverTx: Error delivering tx to ethereum backend", "tx", tx,
			"err", res.Error())
		return res
	}
	app.CollectTx(tx)

	return abciTypes.ResponseDeliverTx{
		Code: abciTypes.CodeTypeOK,
	}
}

// BeginBlock starts a new Ethereum block
// #stable - 0.4.0
func (app *EthermintApplication) BeginBlock(beginBlock abciTypes.RequestBeginBlock) abciTypes.ResponseBeginBlock {

	app.logger.Debug("BeginBlock") // nolint: errcheck

	// update the eth header with the tendermint header
	app.backend.UpdateHeaderWithTimeInfo(beginBlock.GetHeader())
	return abciTypes.ResponseBeginBlock{}
}

// EndBlock accumulates rewards for the validators and updates them
// #stable - 0.4.0
func (app *EthermintApplication) EndBlock(endBlock abciTypes.RequestEndBlock) abciTypes.ResponseEndBlock {

	app.logger.Debug("EndBlock", "height", endBlock.GetHeight()) // nolint: errcheck
	app.backend.AccumulateRewards(app.strategy)

	app.backend.EndBlock()

	return app.GetUpdatedValidators()
}

// Commit commits the block and returns a hash of the current state
// #stable - 0.4.0
func (app *EthermintApplication) Commit() abciTypes.ResponseCommit {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.logger.Debug("Commit") // nolint: errcheck
	blockHash, err := app.backend.Commit(app.Receiver())
	if err != nil {
		// nolint: errcheck
		app.logger.Error("Error getting latest ethereum state", "err", err)
		return abciTypes.ResponseCommit{
			Code: errors.CodeTypeInternalErr,
			Log:  err.Error(),
		}
	}
	state, err := app.getCurrentState()
	if err != nil {
		app.logger.Error("Error getting latest state", "err", err) // nolint: errcheck
		return abciTypes.ResponseCommit{
			Code: errors.CodeTypeInternalErr,
			Log:  err.Error(),
		}
	}

	app.checkTxState = state.Copy()

	app.lowPriceTransactions = make(map[FromTo]*ethTypes.Transaction)

	return abciTypes.ResponseCommit{
		Data: blockHash[:],
	}
}

// Query queries the state of the EthermintApplication
// #stable - 0.4.0
func (app *EthermintApplication) Query(query abciTypes.RequestQuery) abciTypes.ResponseQuery {
	app.logger.Debug("Query") // nolint: errcheck
	var in jsonRequest
	if err := json.Unmarshal(query.Data, &in); err != nil {
		return abciTypes.ResponseQuery{Code: errors.CodeTypeInternalErr,
			Log: err.Error()}
	}
	var result interface{}
	if err := app.rpcClient.Call(&result, in.Method, in.Params...); err != nil {
		return abciTypes.ResponseQuery{Code: errors.CodeTypeInternalErr,
			Log: err.Error()}
	}
	bytes, err := json.Marshal(result)
	if err != nil {
		return abciTypes.ResponseQuery{Code: errors.CodeTypeInternalErr,
			Log: err.Error()}
	}
	return abciTypes.ResponseQuery{Code: abciTypes.CodeTypeOK, Value: bytes}
}

//-------------------------------------------------------

// validateTx checks the validity of a tx against the blockchain's current state.
// it duplicates the logic in ethereum's tx_pool
func (app *EthermintApplication) validateTx(tx *ethTypes.Transaction) abciTypes.ResponseCheckTx {

	// Heuristic limit, reject transactions over 32KB to prevent DOS attacks
	if tx.Size() > maxTransactionSize {
		return abciTypes.ResponseCheckTx{
			Code: errors.CodeTypeInternalErr,
			Log:  core.ErrOversizedData.Error()}
	}

	var signer ethTypes.Signer = ethTypes.FrontierSigner{}
	if tx.Protected() {
		signer = ethTypes.NewEIP155Signer(tx.ChainId())
	}

	// Make sure the transaction is signed properly
	from, err := ethTypes.Sender(signer, tx)
	if err != nil {
		// TODO: Add errors.CodeTypeInvalidSignature ?
		return abciTypes.ResponseCheckTx{
			Code: errors.CodeTypeInternalErr,
			Log:  core.ErrInvalidSender.Error()}
	}

	// Transactions can't be negative. This may never happen using RLP decoded
	// transactions but may occur if you create a transaction using the RPC.
	if tx.Value().Sign() < 0 {
		return abciTypes.ResponseCheckTx{
			Code: errors.CodeTypeBaseInvalidInput,
			Log:  core.ErrNegativeValue.Error()}
	}

	currentState := app.checkTxState

	// Make sure the account exist - cant send from non-existing account.
	if !currentState.Exist(from) {
		return abciTypes.ResponseCheckTx{
			Code: errors.CodeTypeUnknownAddress,
			Log:  core.ErrInvalidSender.Error()}
	}

	// Check the transaction doesn't exceed the current block limit gas.
	gasLimit := app.backend.GasLimit()
	if gasLimit.Cmp(tx.Gas()) < 0 {
		return abciTypes.ResponseCheckTx{
			Code: errors.CodeTypeInternalErr,
			Log:  core.ErrGasLimitReached.Error()}
	}

	nonce := currentState.GetNonce(from)
	if _, ok := utils.NonceCheckedTx[tx.Hash()]; !ok {
		// Check if nonce is not strictly increasing
		// if not then recheck with feeding failed count
		if nonce != tx.Nonce() {
			if c, ok := app.checkFailedCount[from]; ok {
				if nonce+c != tx.Nonce() {
					return abciTypes.ResponseCheckTx{
						Code: errors.CodeTypeBadNonce,
						Log: fmt.Sprintf(
							"Nonce not strictly increasing. Expected %d Got %d",
							nonce, tx.Nonce())}
				}
			} else {
				return abciTypes.ResponseCheckTx{
					Code: errors.CodeTypeBadNonce,
					Log: fmt.Sprintf(
						"Nonce not strictly increasing. Expected %d Got %d",
						nonce, tx.Nonce())}
			}
		}
	}

	// Transactor should have enough funds to cover the costs
	currentBalance := currentState.GetBalance(from)

	// Iterate StateChangeQueue to pre sub the balance
	for _, scObj := range utils.StateChangeQueue {
		if bytes.Equal(from[:], scObj.From.Bytes()) {
			currentBalance.Sub(currentBalance, scObj.Amount)
		}
	}

	// cost == V + GP * GL
	if currentBalance.Cmp(tx.Cost()) < 0 {
		return abciTypes.ResponseCheckTx{
			// TODO: Add errors.CodeTypeInsufficientFunds ?
			Code: errors.CodeTypeBaseInvalidInput,
			Log: fmt.Sprintf(
				"Current balance: %s, tx cost: %s",
				currentBalance, tx.Cost())}
	}

	intrGas := core.IntrinsicGas(tx.Data(), tx.To() == nil, true) // homestead == true
	if tx.Gas().Cmp(intrGas) < 0 {
		return abciTypes.ResponseCheckTx{
			Code: errors.CodeTypeBaseInvalidInput,
			Log:  core.ErrIntrinsicGas.Error()}
	}

	// Iterate over all transactions to check if the gas price is too low for the
	// non-first transaction with the same from/to address
	// Todo performance maybe
	var to common.Address
	if tx.To() != nil {
		to = *tx.To()
	}
	ft := FromTo{
		from: from,
		to:   to,
	}
	if _, ok := app.lowPriceTransactions[ft]; ok {
		if tx.GasPrice().Cmp(big.NewInt(MinGasPrice)) < 0 {
			// add failed count
			// this map will keep growing because the nonce check will use it ongoing
			app.checkFailedCount[from] = app.checkFailedCount[from] + 1
			return abciTypes.ResponseCheckTx{Code: errors.CodeLowGasPriceErr, Log: "The gas price is too low for transaction"}
		}
	}
	if tx.GasPrice().Cmp(big.NewInt(MinGasPrice)) < 0 {
		app.lowPriceTransactions[ft] = tx
	}

	utils.NonceCheckedTx[tx.Hash()] = true

	// Update ether balances
	// amount + gasprice * gaslimit
	currentState.SubBalance(from, tx.Cost())
	// tx.To() returns a pointer to a common address. It returns nil
	// if it is a contract creation transaction.
	if to := tx.To(); to != nil {
		currentState.AddBalance(*to, tx.Value())
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	currentState.SetNonce(from, nonce+1)

	return abciTypes.ResponseCheckTx{Code: abciTypes.CodeTypeOK}
}

