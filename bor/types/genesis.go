package types

import (
	"encoding/json"

	"github.com/maticnetwork/heimdall/helper"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

// GenesisState is the bor state that must be provided at genesis.
type GenesisState struct {
	Params Params          `json:"params" yaml:"params"`
	Spans  []*hmTypes.Span `json:"spans" yaml:"spans"` // list of spans
}

// NewGenesisState creates a new genesis state.
func NewGenesisState(params Params, spans []*hmTypes.Span) GenesisState {
	return GenesisState{
		Params: params,
		Spans:  spans,
	}
}

// DefaultGenesisState returns a default genesis state
func DefaultGenesisState() GenesisState {
	return NewGenesisState(DefaultParams(), nil)
}

// ValidateGenesis performs basic validation of bor genesis data returning an
// error for any failed validation criteria.
func ValidateGenesis(data GenesisState) error {
	if err := data.Params.Validate(); err != nil {
		return err
	}

	return nil
}

// genFirstSpan generates default first valdiator producer set
func genFirstSpan(valset hmTypes.ValidatorSet) []*hmTypes.Span {
	var firstSpan []*hmTypes.Span
	var selectedProducers []hmTypes.Validator
	if len(valset.Validators) > int(DefaultProducerCount) {
		// pop top validators and select
		for i := 0; uint64(i) < DefaultProducerCount; i++ {
			selectedProducers = append(selectedProducers, *valset.Validators[i])
		}
	} else {
		for _, val := range valset.Validators {
			selectedProducers = append(selectedProducers, *val)
		}
	}

	newSpan := hmTypes.NewSpan(0, 0, 0+DefaultSpanDuration-1, valset, selectedProducers, helper.GetConfig().BorChainID)
	firstSpan = append(firstSpan, &newSpan)
	return firstSpan
}

// GetGenesisStateFromAppState returns staking GenesisState given raw application genesis state
func GetGenesisStateFromAppState(appState map[string]json.RawMessage) GenesisState {
	var genesisState GenesisState
	if appState[ModuleName] != nil {
		err := json.Unmarshal(appState[ModuleName], &genesisState)
		if err != nil {
			panic(err)
		}
	}

	return genesisState
}

// SetGenesisStateToAppState sets state into app state
func SetGenesisStateToAppState(appState map[string]json.RawMessage, currentValSet hmTypes.ValidatorSet) (map[string]json.RawMessage, error) {
	// set state to bor state
	borState := GetGenesisStateFromAppState(appState)
	borState.Spans = genFirstSpan(currentValSet)

	var err error
	appState[ModuleName], err = json.Marshal(borState)
	if err != nil {
		return appState, err
	}
	return appState, nil
}
