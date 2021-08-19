package listener

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"time"

	"github.com/RichardKnop/machinery/v1/tasks"
	"github.com/maticnetwork/bor/accounts/abi"
	ethTypes "github.com/maticnetwork/bor/core/types"
	"github.com/maticnetwork/heimdall/bridge/setu/util"
	chainmanagerTypes "github.com/maticnetwork/heimdall/chainmanager/types"
	"github.com/maticnetwork/heimdall/contracts/stakinginfo"
	"github.com/maticnetwork/heimdall/helper"
	"github.com/maticnetwork/heimdall/types"
)

const (
	tronLastBlockKey = "tron-last-block" // storage key
)

// TronListener - Listens to and process events from Tron
type TronListener struct {
	BaseListener
	rootChainType string
	// ABIs
	abis           []*abi.ABI
	stakingInfoAbi *abi.ABI
}

// NewTronListener - constructor func
func NewTronListener() *TronListener {
	contractCaller, err := helper.NewContractCaller()
	if err != nil {
		panic(err)
	}
	TronListener := &TronListener{
		rootChainType: types.RootChainTypeTron,
		abis: []*abi.ABI{
			&contractCaller.RootChainABI,
			&contractCaller.StateSenderABI,
			&contractCaller.StakingInfoABI,
		},
		stakingInfoAbi: &contractCaller.StakingInfoABI,
	}

	return TronListener
}

// Start starts new block subscription
func (tl *TronListener) Start() error {
	tl.Logger.Info("Starting")

	// create cancellable context
	headerCtx, cancelHeaderProcess := context.WithCancel(context.Background())
	tl.cancelHeaderProcess = cancelHeaderProcess

	// start header process
	go tl.StartHeaderProcess(headerCtx)

	// subscribe to new head
	pollInterval := helper.GetConfig().SyncerPollInterval

	tl.Logger.Info("Start polling for events", "pollInterval", pollInterval)
	// poll for new header using client object
	go tl.StartPolling(headerCtx, pollInterval)
	return nil
}

// startPolling starts polling
func (tl *TronListener) StartPolling(ctx context.Context, pollInterval time.Duration) {
	// How often to fire the passed in function in second
	interval := pollInterval

	// Setup the ticket and the channel to signal
	// the ending of the interval
	ticker := time.NewTicker(interval)

	// start listening
	for {
		select {
		case <-ticker.C:
			headerNum, err := tl.contractConnector.GetTronLatestBlockNumber()
			if err == nil {
				// send data to channel
				tl.HeaderChannel <- &(ethTypes.Header{
					Number: big.NewInt(headerNum),
				})
			}
		case <-ctx.Done():
			tl.Logger.Info("Polling stopped")
			ticker.Stop()
			return
		}
	}
}

// ProcessHeader - process headerblock from rootchain
func (tl *TronListener) ProcessHeader(newHeader *ethTypes.Header) {
	tl.Logger.Debug("New block detected", "blockNumber", newHeader.Number)

	// fetch context
	chainManagerParams, err := tl.getChainManagerParams()
	if err != nil {
		return
	}
	latestNumber := newHeader.Number
	// confirmation
	confirmationBlocks := big.NewInt(int64(chainManagerParams.TronchainTxConfirmations))

	if latestNumber.Cmp(confirmationBlocks) <= 0 {
		tl.Logger.Error("Block number less than Confirmations required", "blockNumber", latestNumber.Uint64, "confirmationsRequired", confirmationBlocks.Uint64)
		return
	}
	latestNumber = latestNumber.Sub(latestNumber, confirmationBlocks)

	// default fromBlock
	fromBlock := latestNumber

	// get last block from storage
	hasLastBlock, _ := tl.storageClient.Has([]byte(tronLastBlockKey), nil)
	if hasLastBlock {
		lastBlockBytes, err := tl.storageClient.Get([]byte(tronLastBlockKey), nil)
		if err != nil {
			tl.Logger.Info("Error while fetching last block bytes from storage", "error", err)
			return
		}
		tl.Logger.Debug("Got last block from bridge storage", "lastBlock", string(lastBlockBytes))
		if result, err := strconv.ParseUint(string(lastBlockBytes), 10, 64); err == nil {
			if result >= newHeader.Number.Uint64() {
				return
			}
			fromBlock = big.NewInt(0).SetUint64(result + 1)
		}
	}

	// to block
	toBlock := latestNumber

	if toBlock.Cmp(fromBlock) == -1 {
		fromBlock = toBlock
	}

	// set last block to storage
	if err := tl.storageClient.Put([]byte(tronLastBlockKey), []byte(toBlock.String()), nil); err != nil {
		tl.Logger.Error("tl.storageClient.Put", "Error", err)
	}

	// query events
	tl.queryAndBroadcastEvents(chainManagerParams, fromBlock, toBlock)
}

func (tl *TronListener) queryAndBroadcastEvents(chainManagerParams *chainmanagerTypes.Params, fromBlock *big.Int, toBlock *big.Int) {
	tl.Logger.Info("Query tron event logs", "fromBlock", fromBlock, "toBlock", toBlock)

	var tronContractAddresses []string
	tronContractAddresses = append(tronContractAddresses, chainManagerParams.ChainParams.TronStateSenderAddress)
	tronContractAddresses = append(tronContractAddresses, chainManagerParams.ChainParams.TronChainAddress)
	tronContractAddresses = append(tronContractAddresses, chainManagerParams.ChainParams.TronStateInfoAddress)
	// current public key
	pubkeyBytes := helper.GetPubKey().Bytes()
	logs, err := tl.contractConnector.GetTronEventsByContractAddress(tronContractAddresses, fromBlock.Int64(), toBlock.Int64())

	if err != nil {
		tl.Logger.Error("Error while query tron logs", "error", err)
		return
	} else if len(logs) > 0 {
		tl.Logger.Debug("New tron logs found", "numberOfLogs", len(logs))
	}
	// process filtered log
	for _, vLog := range logs {
		topic := vLog.Topics[0].Bytes()
		for _, abiObject := range tl.abis {
			selectedEvent := helper.EventByID(abiObject, topic)
			logBytes, _ := json.Marshal(vLog)
			if selectedEvent != nil {
				tl.Logger.Debug("ReceivedTronEvent", "eventname", selectedEvent.Name)
				switch selectedEvent.Name {
				case "NewHeaderBlock":
					if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
						tl.sendTaskWithDelay("sendCheckpointAckToHeimdall", selectedEvent.Name, logBytes, delay)
					}
				case "Staked":
					if tl.rootChainType == types.RootChainTypeStake {
						event := new(stakinginfo.StakinginfoStaked)
						if err := helper.UnpackLog(tl.stakingInfoAbi, event, selectedEvent.Name, &vLog); err != nil {
							tl.Logger.Error("Error while parsing tron event", "name", selectedEvent.Name, "error", err)
						}
						if bytes.Equal(event.SignerPubkey, pubkeyBytes) {
							// topup has to be processed first before validator join. so adding delay.
							delay := util.TaskDelayBetweenEachVal
							tl.sendTaskWithDelay("sendValidatorJoinToHeimdall", selectedEvent.Name, logBytes, delay)
						} else if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							// topup has to be processed first before validator join. so adding delay.
							delay = delay + util.TaskDelayBetweenEachVal
							tl.sendTaskWithDelay("sendValidatorJoinToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}

				case "StakeUpdate":
					if tl.rootChainType == types.RootChainTypeStake {
						event := new(stakinginfo.StakinginfoStakeUpdate)
						if err := helper.UnpackLog(tl.stakingInfoAbi, event, selectedEvent.Name, &vLog); err != nil {
							tl.Logger.Error("Error while parsing tron event", "name", selectedEvent.Name, "error", err)
						}
						if util.IsEventSender(tl.cliCtx, event.ValidatorId.Uint64()) {
							tl.sendTaskWithDelay("sendStakeUpdateToHeimdall", selectedEvent.Name, logBytes, 0)
						} else if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendStakeUpdateToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}
				case "SignerChange":
					if tl.rootChainType == types.RootChainTypeStake {
						event := new(stakinginfo.StakinginfoSignerChange)
						if err := helper.UnpackLog(tl.stakingInfoAbi, event, selectedEvent.Name, &vLog); err != nil {
							tl.Logger.Error("Error while parsing tron event", "name", selectedEvent.Name, "error", err)
						}
						if bytes.Equal(event.SignerPubkey, pubkeyBytes) {
							tl.sendTaskWithDelay("sendSignerChangeToHeimdall", selectedEvent.Name, logBytes, 0)
						} else if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendSignerChangeToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}

				case "UnstakeInit":
					if tl.rootChainType == types.RootChainTypeStake {
						event := new(stakinginfo.StakinginfoUnstakeInit)
						if err := helper.UnpackLog(tl.stakingInfoAbi, event, selectedEvent.Name, &vLog); err != nil {
							tl.Logger.Error("Error while parsing tron event", "name", selectedEvent.Name, "error", err)
						}
						if util.IsEventSender(tl.cliCtx, event.ValidatorId.Uint64()) {
							tl.sendTaskWithDelay("sendUnstakeInitToHeimdall", selectedEvent.Name, logBytes, 0)
						} else if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendUnstakeInitToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}

				case "StateSynced":
					if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
						tl.sendTaskWithDelay("sendStateSyncedToHeimdall", selectedEvent.Name, logBytes, delay)
					}

				case "TopUpFee":
					if tl.rootChainType == types.RootChainTypeStake {
						event := new(stakinginfo.StakinginfoTopUpFee)
						if err := helper.UnpackLog(tl.stakingInfoAbi, event, selectedEvent.Name, &vLog); err != nil {
							tl.Logger.Error("Error while parsing tron event", "name", selectedEvent.Name, "error", err)
						}
						if bytes.Equal(event.User.Bytes(), helper.GetAddress()) {
							tl.sendTaskWithDelay("sendTopUpFeeToHeimdall", selectedEvent.Name, logBytes, 0)
						} else if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendTopUpFeeToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}

				case "Slashed":
					if tl.rootChainType == types.RootChainTypeStake {
						if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendTickAckToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}

				case "UnJailed":
					if tl.rootChainType == types.RootChainTypeStake {
						event := new(stakinginfo.StakinginfoUnJailed)
						if err := helper.UnpackLog(tl.stakingInfoAbi, event, selectedEvent.Name, &vLog); err != nil {
							tl.Logger.Error("Error while parsing tron event", "name", selectedEvent.Name, "error", err)
						}
						if util.IsEventSender(tl.cliCtx, event.ValidatorId.Uint64()) {
							tl.sendTaskWithDelay("sendUnjailToHeimdall", selectedEvent.Name, logBytes, 0)
						} else if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendUnjailToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}

				case "StakeAck":
					if tl.rootChainType != types.RootChainTypeStake {
						if isCurrentValidator, delay := util.CalculateTaskDelay(tl.cliCtx); isCurrentValidator {
							tl.sendTaskWithDelay("sendStakingAckToHeimdall", selectedEvent.Name, logBytes, delay)
						}
					}
				}
			}
		}
	}

}

func (tl *TronListener) sendTaskWithDelay(taskName string, eventName string, eventBytes []byte, delay time.Duration) {
	signature := &tasks.Signature{
		Name: taskName,
		Args: []tasks.Arg{
			{
				Type:  "string",
				Value: eventName,
			},
			{
				Type:  "string",
				Value: string(eventBytes),
			},
			{
				Type:  "string",
				Value: tl.rootChainType,
			},
		},
	}
	signature.RetryCount = 3

	// add delay for task so that multiple validators won't send same transaction at same time
	eta := time.Now().Add(delay)
	signature.ETA = &eta
	tl.Logger.Info("Sending tron task", "taskName", taskName, "currentTime", time.Now(), "delayTime", eta)
	_, err := tl.queueConnector.Server.SendTask(signature)
	if err != nil {
		tl.Logger.Error("Error sending tron task", "taskName", taskName, "error", err)
	}
}

//
// utils
//

func (tl *TronListener) getChainManagerParams() (*chainmanagerTypes.Params, error) {
	chainmanagerParams, err := util.GetChainmanagerParams(tl.cliCtx)
	if err != nil {
		tl.Logger.Error("Error while fetching tron  chain manager params", "error", err)
		return nil, err
	}

	return chainmanagerParams, nil
}
