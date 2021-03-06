package model

import (
	"fmt"
	"strings"

	"github.com/svanas/nefertiti/pricing"
)

type Market struct {
	Name  string `json:"name"`
	Base  string `json:"-"`
	Quote string `json:"-"`
}

func GetBaseCurr(markets []Market, market string) (string, error) {
	for _, m := range markets {
		if m.Name == market {
			return m.Base, nil
		}
	}
	return "", fmt.Errorf("market %s does not exist", market)
}

func GetQuoteCurr(markets []Market, market string) (string, error) {
	for _, m := range markets {
		if m.Name == market {
			return m.Quote, nil
		}
	}
	return "", fmt.Errorf("market %s does not exist", market)
}

func ParseMarket(markets []Market, market string) (base, quote string, err error) {
	for _, m := range markets {
		if m.Name == market {
			return m.Base, m.Quote, nil
		}
	}
	return "", "", fmt.Errorf("market %s does not exist", market)
}

func TweetMarket(markets []Market, market string) string {
	i := IndexByMarket(markets, market)
	if i > -1 {
		return fmt.Sprintf("$%s-%s", strings.ToUpper(markets[i].Base), strings.ToUpper(markets[i].Quote))
	}
	return market
}

func IndexByMarket(markets []Market, market string) int {
	if markets != nil {
		for i, m := range markets {
			if m.Name == market {
				return i
			}
		}
	}
	return -1
}

func HasMarket(markets []Market, market string) bool {
	return IndexByMarket(markets, market) > -1
}

// GetSizeMin returns the minimum size we must BUY, source: https://pro.coinbase.com/markets
func GetSizeMin(hold bool, asset string) float64 {
	if hold {
		if strings.EqualFold(asset, BTC) {
			return 0.0001
		}
		if strings.EqualFold(asset, ETH) {
			return 0.001
		}
	}
	return 0
}

// GetSizeMax returns the maximum size we can SELL
func GetSizeMax(hold bool, def float64, prec func() int) float64 {
	if hold {
		// when we hodl, we then sell 20% of the purchased amount
		return pricing.RoundToPrecision((def * 0.20), prec())
	}
	return def
}

type (
	Markets []string
)

func (markets Markets) IndexOf(market string) int {
	for i, m := range markets {
		if m == market {
			return i
		}
	}
	return -1
}

func (markets Markets) HasMarket(market string) bool {
	return markets.IndexOf(market) > -1
}

type (
	Assets []string
)

func (assets Assets) IndexOf(asset string) int {
	for i, a := range assets {
		if strings.EqualFold(a, asset) {
			return i
		}
	}
	return -1
}

func (assets Assets) HasAsset(asset string) bool {
	return assets.IndexOf(asset) > -1
}

func (assets Assets) IsEmpty() bool {
	return len(assets) == 0 || len(assets) == 1 && assets[0] == ""
}
