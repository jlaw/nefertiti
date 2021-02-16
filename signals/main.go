package signals

import (
	"strings"

	"github.com/svanas/nefertiti/model"
)

type Signals []model.Channel

func (signals *Signals) FindByName(name string) (model.Channel, error) {
	for _, channel := range *signals {
		if strings.EqualFold(channel.GetName(), name) {
			err := channel.Init()
			return channel, err
		}
	}
	return nil, nil
}

func New() *Signals {
	var out Signals
	out = append(out, NewCryptoBaseScanner())
	out = append(out, NewListings())
	out = append(out, NewMiningHamster())
	out = append(out, NewVolume())
	return &out
}
