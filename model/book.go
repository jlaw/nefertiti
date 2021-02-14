package model

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"strconv"

	"github.com/go-errors/errors"
)

type (
	Buy struct {
		Market string  `json:"market"`
		Price  float64 `json:"price"`
	}
	Buys []Buy
)

type (
	Call struct {
		*Buy
		Skip   bool   `json:"-"`
		Stop   string `json:"stop,omitempty"`
		Target string `json:"target,omitempty"`
	}
	Calls []Call
)

func Call2File(call *Call, name string) error {
	var (
		err error
		raw []byte
	)
	if raw, err = json.Marshal(call); err != nil {
		return errors.Wrap(err, 1)
	}
	if _, err = os.Stat(name); err == nil {
		if err = os.Truncate(name, 0); err != nil {
			return errors.Wrap(err, 1)
		}
	}
	if err = ioutil.WriteFile(name, raw, 0600); err != nil {
		return errors.Wrap(err, 1)
	}
	return nil
}

func File2Call(name string) (*Call, error) {
	var (
		err error
		raw []byte
		out Call
	)
	if raw, err = ioutil.ReadFile(name); err != nil {
		return nil, errors.Wrap(err, 1)
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, errors.Wrap(err, 1)
	}
	return &out, nil
}

func (c *Call) HasStop() bool {
	return c.Stop != "" && c.ParseStop() > 0
}

func (c *Call) ParseStop() float64 {
	out, err := strconv.ParseFloat(c.Stop, 64)
	if err == nil {
		return out
	}
	return 0
}

func (c *Call) HasTarget() bool {
	return c.Target != "" && c.ParseTarget() > 0
}

func (c *Call) ParseTarget() float64 {
	out, err := strconv.ParseFloat(c.Target, 64)
	if err == nil {
		return out
	}
	return 0
}

func (c *Call) Corrupt(orderType OrderType) bool {
	// limit order without a limit? then ignore this signal.
	if c.Price == 0 && orderType == LIMIT {
		return true
	}
	if c.HasTarget() {
		// is the target lower than the buy zone? then ignore this signal.
		if c.ParseTarget() < c.Price {
			return true
		}
	}
	return false
}

func (c Calls) HasBuy() bool {
	for _, e := range c {
		if !e.Skip {
			return true
		}
	}
	return false
}

func (c Calls) IndexByMarket(market string) int {
	for i, e := range c {
		if e.Market == market {
			return i
		}
	}
	return -1
}

func (c Calls) IndexByPrice(price float64) int {
	for i, e := range c {
		if e.Price == price {
			return i
		}
	}
	return -1
}

func (c Calls) IndexByMarketPrice(market string, price float64) int {
	for i, e := range c {
		if e.Market == market && e.Price == price {
			return i
		}
	}
	return -1
}

type (
	BookEntry struct {
		*Buy
		Size float64 `json:"size"`
	}
	Book []BookEntry
)

type BookSide int

const (
	BOOK_SIDE_BIDS BookSide = iota
	BOOK_SIDE_ASKS
)

func (b Book) Calls() Calls {
	var out Calls
	for _, e := range b {
		out = append(out, Call{
			Buy: &Buy{
				Market: e.Market,
				Price:  e.Price,
			},
		})
	}
	return out
}

func (b Book) IndexByPrice(price float64) int {
	for i, e := range b {
		if e.Price == price {
			return i
		}
	}
	return -1
}

func (b Book) EntryByPrice(price float64) *BookEntry {
	i := b.IndexByPrice(price)
	if i != -1 {
		return &b[i]
	}
	return nil
}
