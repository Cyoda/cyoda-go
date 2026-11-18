package dispatch

import (
	"errors"
	"math/rand"

	"github.com/cyoda/cyoda-go/internal/contract"
)

type PeerSelector interface {
	Select(candidates []contract.NodeInfo) (contract.NodeInfo, error)
}

type RandomSelector struct{}

func NewRandomSelector() *RandomSelector { return &RandomSelector{} }

func (s *RandomSelector) Select(candidates []contract.NodeInfo) (contract.NodeInfo, error) {
	if len(candidates) == 0 {
		return contract.NodeInfo{}, errors.New("no candidates")
	}
	return candidates[rand.Intn(len(candidates))], nil
}
