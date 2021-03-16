package exchanges

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/alexflint/go-filemutex"
	"github.com/go-errors/errors"
	exchange "github.com/svanas/nefertiti/bittrex"
	"github.com/svanas/nefertiti/flag"
	"github.com/svanas/nefertiti/model"
	"github.com/svanas/nefertiti/notify"
	"github.com/svanas/nefertiti/pricing"
	"github.com/svanas/nefertiti/session"
)

const (
	BITTREX_APP_ID = "214"
)

var (
	bittrexMutex *filemutex.FileMutex
)

const (
	bittrexSessionFile = "bittrex.time"
	bittrexSessionLock = "bittrex.lock"
	bittrexSessionInfo = "bittrex.json"
)

type BittrexSessionInfo struct {
	Cooldown bool            `json:"cooldown"`
	Calls    []exchange.Call `json:"calls"`
}

func bittrexRequestsPerSecond(path string) (float64, bool) { // -> (rps, cooldown)
	var (
		err  error
		data []byte
		info BittrexSessionInfo
	)
	data, err = ioutil.ReadFile(session.GetSessionFile(bittrexSessionInfo))
	if err != nil {
		info.Calls = exchange.Calls
	} else {
		err = json.Unmarshal(data, &info)
		if err != nil {
			info.Calls = exchange.Calls
		} else {
			if info.Cooldown {
				info.Cooldown = false
				if data, err = json.Marshal(info); err == nil {
					err = ioutil.WriteFile(session.GetSessionFile(bittrexSessionInfo), data, 0600)
				}
				return exchange.RequestsPerSecond(exchange.INTENSITY_SUPER), true
			}
		}
	}
	for i := range path {
		if strings.Index("?", string(path[i])) > -1 {
			path = path[:i]
			break
		}
	}
	for _, call := range info.Calls {
		if call.Path == path {
			return exchange.RequestsPerSecond(call.Intensity), false
		}
	}
	return exchange.RequestsPerSecond(exchange.INTENSITY_LOW), false
}

func init() {
	// BeforeRequest
	exchange.BeforeRequest = func(path string) (bool, error) {
		var (
			err    error
			rps    float64
			cooled bool = false
		)

		if bittrexMutex == nil {
			if bittrexMutex, err = filemutex.New(session.GetSessionFile(bittrexSessionLock)); err != nil {
				return cooled, err
			}
		}

		if err = bittrexMutex.Lock(); err != nil {
			return cooled, err
		}

		var lastRequest *time.Time
		if lastRequest, err = session.GetLastRequest(bittrexSessionFile); err != nil {
			return cooled, err
		}

		if lastRequest != nil {
			elapsed := time.Since(*lastRequest)
			rps, cooled = bittrexRequestsPerSecond(path)
			if elapsed.Seconds() < (float64(1) / rps) {
				sleep := time.Duration((float64(time.Second) / rps)) - elapsed
				if flag.Debug() {
					log.Printf("[DEBUG] sleeping %f seconds", sleep.Seconds())
				}
				time.Sleep(sleep)
			}
		}

		if flag.Debug() {
			log.Println("[DEBUG] GET " + path)
		}

		return cooled, nil
	}
	// AfterRequest
	exchange.AfterRequest = func() {
		defer func() {
			bittrexMutex.Unlock()
		}()
		session.SetLastRequest(bittrexSessionFile, time.Now())
	}
	// HandleRateLimitErr
	exchange.HandleRateLimitErr = func(path string, cooled bool) error {
		var (
			err    error
			data   []byte
			info   BittrexSessionInfo
			exists bool
		)
		data, err = ioutil.ReadFile(session.GetSessionFile(bittrexSessionInfo))
		if err != nil {
			info.Calls = exchange.Calls
		} else {
			err = json.Unmarshal(data, &info)
			if err != nil {
				info.Calls = exchange.Calls
			}
		}
		for idx := range path {
			if strings.Index("?", string(path[idx])) > -1 {
				path = path[:idx]
				break
			}
		}
		for idx := range info.Calls {
			if info.Calls[idx].Path == path {
				if cooled {
					// rate limited immediately after a cooldown?
					// 1. do another round of "cooling down"
					// 2. do not slow this endpoint down just yet.
				} else {
					info.Calls[idx].Intensity = info.Calls[idx].Intensity + 1
				}
				exists = true
			}
		}
		if !exists {
			info.Calls = append(info.Calls, exchange.Call{
				Path:      path,
				Intensity: exchange.INTENSITY_TWO,
			})
		}
		info.Cooldown = true
		if data, err = json.Marshal(info); err == nil {
			err = ioutil.WriteFile(session.GetSessionFile(bittrexSessionInfo), data, 0600)
		}
		return err
	}
}

// ----------------------------- private globals ------------------------------

func bittrexOrderSide(order *exchange.Order) model.OrderSide {
	if order.Direction == exchange.OrderSideString[exchange.BUY] {
		return model.BUY
	} else if order.Direction == exchange.OrderSideString[exchange.SELL] {
		return model.SELL
	}
	return model.ORDER_SIDE_NONE
}

func bittrexParseMarket(market string, version int) (base, quote string, err error) {
	symbols := strings.Split(market, "-")
	if len(symbols) > 1 {
		if version >= 3 {
			return symbols[0], symbols[1], nil
		} else {
			return symbols[1], symbols[0], nil
		}
	}
	return "", "", errors.Errorf("Cannot parse market %s", market)
}

func bittrexLogErr(err error, level int64, service model.Notify) {
	pc, file, line, _ := runtime.Caller(1)
	prefix := errors.FormatCaller(pc, file, line)

	msg := fmt.Sprintf("%s %v", prefix, err)
	_, ok := err.(*errors.Error)
	if ok && flag.Debug() {
		log.Printf("[ERROR] %s", err.(*errors.Error).ErrorStack(prefix, ""))
	} else {
		log.Printf("[ERROR] %s", msg)
	}

	if service != nil {
		if notify.CanSend(level, notify.ERROR) {
			err := service.SendMessage(msg, "Bittrex - ERROR")
			if err != nil {
				log.Printf("[ERROR] %v", err)
			}
		}
	}
}

func bittrexLogErrEx(err error, order *exchange.Order, level int64, service model.Notify) {
	pc, file, line, _ := runtime.Caller(1)
	prefix := errors.FormatCaller(pc, file, line)
	msg := fmt.Sprintf("%s %v", prefix, err)

	if service != nil {
		if notify.CanSend(level, notify.ERROR) {
			err := service.SendMessage(msg, "Bittrex - ERROR")
			if err != nil {
				log.Printf("[ERROR] %v", err)
			}
		}
	}

	_, ok := err.(*errors.Error)
	if ok && flag.Debug() {
		if order != nil {
			var data []byte
			if data, _ = json.Marshal(order); data != nil {
				log.Printf("[ERROR] %s", errors.Append(err, "\t", string(data)).ErrorStack(prefix, ""))
				return
			}
		}
		log.Printf("[ERROR] %s", err.(*errors.Error).ErrorStack(prefix, ""))
		return
	}

	log.Printf("[ERROR] %s", msg)
}

func bittrexCancelOrder(client *exchange.Client, order *exchange.Order) (float64, error) { // -> (ocoTriggerPrice, error)
	var (
		err          error
		triggerPrice float64
		conditionals []exchange.ConditionalOrder
	)
	// get conditional orders
	if conditionals, err = client.GetOpenConditionalOrders(order.MarketSymbol); err != nil {
		return 0, err
	}
	// is this order referenced by a conditional order?
	for _, conditional := range conditionals {
		if conditional.OrderToCancel != nil {
			if conditional.OrderToCancel.Id == order.Id {
				// if yes, cancel the conditional order
				triggerPrice = conditional.TriggerPrice
				if err = client.CancelConditionalOrder(conditional.Id); err != nil {
					return triggerPrice, err
				}
			}
		}
	}
	// last but not least, cancel this order
	return triggerPrice, client.CancelOrder(order.Id)
}

// ----------------------------------------------------------------------------

type Bittrex struct {
	*model.ExchangeInfo
	markets []exchange.Market
}

func (self *Bittrex) GetInfo() *model.ExchangeInfo {
	return self.ExchangeInfo
}

func (self *Bittrex) GetClient(permission model.Permission, sandbox bool) (interface{}, error) {
	if permission != model.PRIVATE {
		return exchange.New("", "", BITTREX_APP_ID), nil
	}

	apiKey, apiSecret, err := promptForApiKeys("Bittrex")
	if err != nil {
		return nil, err
	}

	return exchange.New(apiKey, apiSecret, BITTREX_APP_ID), nil
}

func (self *Bittrex) GetMarkets(cached, sandbox bool) ([]model.Market, error) {
	var (
		err error
		out []model.Market
	)

	if self.markets == nil || cached == false {
		client := exchange.New("", "", BITTREX_APP_ID)
		if self.markets, err = client.GetMarkets(); err != nil {
			return nil, errors.Wrap(err, 1)
		}
	}

	for _, market := range self.markets {
		if market.Online() {
			out = append(out, model.Market{
				Name:  market.MarketName(),
				Base:  market.BaseCurrencySymbol,
				Quote: market.QuoteCurrencySymbol,
			})
		}
	}

	return out, nil
}

func (self *Bittrex) FormatMarket(base, quote string) string {
	return strings.ToUpper(fmt.Sprintf("%s-%s", quote, base))
}

func (self *Bittrex) formatMarketEx(base, quote string, version int) string {
	if version >= 3 {
		return strings.ToUpper(fmt.Sprintf("%s-%s", base, quote))
	} else {
		return strings.ToUpper(fmt.Sprintf("%s-%s", quote, base))
	}
}

// ConvertMarket converts a market from the old version to version 3.
func (self *Bittrex) convertMarket(old string) (string, error) {
	var (
		err   error
		base  string
		quote string
	)
	if base, quote, err = bittrexParseMarket(old, 1); err != nil {
		return "", err
	}
	return self.formatMarketEx(base, quote, 3), nil
}

// listens to the open orders, look for cancelled orders, send a notification.
func (self *Bittrex) listen(
	client *exchange.Client,
	service model.Notify,
	level int64,
	old exchange.Orders,
	history exchange.Orders,
) (exchange.Orders, error) {
	var err error

	// get my new open orders
	var new exchange.Orders
	if new, err = client.GetOpenOrders("all"); err != nil {
		return old, errors.Wrap(err, 1)
	}

	// look for cancelled orders
	for _, order := range old {
		if new.IndexByOrderId(order.Id) == -1 {
			// if this order has NOT been FILLED, then it has been cancelled.
			if history.IndexByOrderId(order.Id) == -1 {
				var data []byte
				if data, err = json.Marshal(order); err != nil {
					return new, errors.Wrap(err, 1)
				}

				log.Println("[CANCELLED] " + string(data))

				side := bittrexOrderSide(&order)
				if side != model.ORDER_SIDE_NONE {
					if service != nil && notify.CanSend(level, notify.CANCELLED) {
						if err = service.SendMessage(string(data), fmt.Sprintf("Bittrex - Done %s (Reason: Cancelled)", model.FormatOrderSide(side))); err != nil {
							log.Printf("[ERROR] %v", err)
						}
					}
				}
			}
		}
	}

	// look for new orders
	for _, order := range new {
		if old.IndexByOrderId(order.Id) == -1 {
			var data []byte
			if data, err = json.Marshal(order); err != nil {
				return new, errors.Wrap(err, 1)
			}

			log.Println("[OPEN] " + string(data))

			side := bittrexOrderSide(&order)
			if side != model.ORDER_SIDE_NONE {
				// [BUG] every now and then, Bittrex is sending out Open Sell notification(s) for previously sold order(s). Here we single those out.
				if side != model.SELL || history.IndexByOrderIdEx(order.Id, exchange.SELL) == -1 {
					if service != nil && (notify.CanSend(level, notify.OPENED) || (level == notify.LEVEL_DEFAULT && side == model.SELL)) {
						if err = service.SendMessage(string(data), ("Bittrex - Open " + model.FormatOrderSide(side))); err != nil {
							log.Printf("[ERROR] %v", err)
						}
					}
				}
			}
		}
	}

	return new, nil
}

// listens to the order history, look for newly filled orders, automatically place new LIMIT SELL orders.
func (self *Bittrex) sell(
	client *exchange.Client,
	strategy model.Strategy,
	mult float64,
	hold model.Markets,
	service model.Notify,
	twitter *notify.TwitterKeys,
	level int64,
	old exchange.Orders,
	sandbox bool,
) (exchange.Orders, error) {
	var (
		err     error
		markets []model.Market
	)

	if markets, err = self.GetMarkets(true, sandbox); err != nil {
		return old, err
	}

	// get my new order history
	var new exchange.Orders
	if new, err = client.GetOrderHistory("all"); err != nil {
		return old, errors.Wrap(err, 1)
	}

	// look for filled orders
	for _, order := range new {
		if old.IndexByOrderId(order.Id) == -1 {
			var data []byte
			if data, err = json.Marshal(order); err != nil {
				return new, errors.Wrap(err, 1)
			}

			log.Println("[FILLED] " + string(data))

			side := bittrexOrderSide(&order)
			if side != model.ORDER_SIDE_NONE {
				// send notification(s)
				if notify.CanSend(level, notify.FILLED) {
					if service != nil {
						title := fmt.Sprintf("Bittrex - Done %s", model.FormatOrderSide(side))
						if side == model.SELL {
							if strategy == model.STRATEGY_STOP_LOSS && order.Type() == exchange.MARKET {
								title = fmt.Sprintf("%s -%.2f%%", title, (((mult * 2) - 1) * 100))
							} else {
								title = fmt.Sprintf("%s +%.2f%%", title, ((mult - 1) * 100))
							}
						}
						if err = service.SendMessage(string(data), title); err != nil {
							log.Printf("[ERROR] %v", err)
						}
					}
					if twitter != nil {
						notify.Tweet(twitter, fmt.Sprintf("Done %s. %s priced at %f #Bittrex", model.FormatOrderSide(side), model.TweetMarket(markets, order.MarketName()), order.Price()))
					}
				}

				// has a stop loss been filled? then place a buy order double the order size *** if --dca is included ***
				if side == model.SELL {
					if strategy == model.STRATEGY_STOP_LOSS && order.Type() == exchange.MARKET {
						if flag.Exists("dca") {
							var prec int
							if prec, err = self.GetSizePrec(client, order.MarketName()); err == nil {
								size := 2 * order.QuantityFilled()
								_, _, err = self.Order(client,
									model.BUY,
									order.MarketName(),
									pricing.RoundToPrecision(size, prec),
									0, model.MARKET, "",
								)
							}
							if err != nil {
								return new, errors.Append(err, "\t", string(data))
							}
						}
					}
				}

				// has a buy order been filled? then place a sell order
				if side == model.BUY {
					var (
						base  string
						quote string
					)
					base, quote, err = model.ParseMarket(markets, order.MarketName())
					// --- BEGIN --- svanas 2021-05-28 --- do not error on new listings ---
					if err != nil {
						markets, err = self.GetMarkets(false, sandbox)
						if err == nil {
							base, quote, err = model.ParseMarket(markets, order.MarketName())
						}
					}
					// ---- END ---- svanas 2021-05-28 ------------------------------------
					if err == nil {
						var prec int
						if prec, err = self.GetPricePrec(client, order.MarketName()); err == nil {
							amount := order.QuantityFilled()
							// only sell to break even and bag the remaining amount
							if flag.Exists("bag") {
								sp,_ := self.GetSizePrec(client, order.MarketName())
								amount = pricing.FloorToPrecision(order.QuantityFilled() / mult, sp)
								log.Printf("[INFO] Sell %f to BE", amount)
							}
							qty := self.GetMaxSize(client, base, quote, hold.HasMarket(order.MarketName()), amount)
							if qty > 0 {
								tgt := pricing.Multiply(order.Price(), mult, prec)
								if strategy == model.STRATEGY_STOP_LOSS {
									stop := order.Price() / pricing.NewMult(mult, 2.0)
									_, err = self.OCO(
										client,
										order.MarketName(),
										qty,
										tgt,
										pricing.RoundToPrecision(stop, prec),
										"", "",
									)
								} else {
									_, _, err = self.Order(
										client, model.SELL,
										order.MarketName(),
										qty,
										tgt,
										model.LIMIT, "",
									)
								}
							}
						}
					}
					if err != nil {
						return new, errors.Append(err, "\t", string(data))
					}
				}
			}
		}
	}

	return new, nil
}

func (self *Bittrex) Sell(
	start time.Time,
	hold model.Markets,
	sandbox, tweet, debug bool,
	success model.OnSuccess,
) error {
	var err error

	strategy := model.GetStrategy()
	if strategy == model.STRATEGY_STANDARD || strategy == model.STRATEGY_STOP_LOSS {
		// we are OK
	} else {
		return errors.New("Strategy not implemented")
	}

	var (
		apiKey    string
		apiSecret string
	)
	if apiKey, apiSecret, err = promptForApiKeys("Bittrex"); err != nil {
		return err
	}

	var service model.Notify = nil
	if service, err = notify.New().Init(flag.Interactive(), true); err != nil {
		return err
	}

	var twitter *notify.TwitterKeys = nil
	if tweet {
		if twitter, err = notify.TwitterPromptForKeys(flag.Interactive()); err != nil {
			return err
		}
	}

	client := exchange.New(apiKey, apiSecret, BITTREX_APP_ID)

	// get my order history
	var history exchange.Orders
	if history, err = client.GetOrderHistory("all"); err != nil {
		return errors.Wrap(err, 1)
	}

	// get my open orders
	var open exchange.Orders
	if open, err = client.GetOpenOrders("all"); err != nil {
		return errors.Wrap(err, 1)
	}

	if err = success(service); err != nil {
		return err
	}

	reopenedAt := time.Now()
	const reopenAfterDays = 21

	for {
		// read the dynamic settings
		var (
			level    int64          = notify.Level()
			mult     float64        = model.GetMult()
			strategy model.Strategy = model.GetStrategy()
		)
		// listens to the order history, look for newly filled orders, automatically place new LIMIT SELL orders.
		history, err = self.sell(client, strategy, mult, hold, service, twitter, level, history, sandbox)
		if err != nil {
			bittrexLogErr(err, level, service)
		} else {
			// listens to the open orders, look for cancelled orders, send a notification.
			open, err = self.listen(client, service, level, open, history)
			if err != nil {
				bittrexLogErr(err, level, service)
			} else {
				// Effective 25-nov-2017, Bittrex will be removing orders that are older than 28 days. Here we will...
				// 1. check for those every hour, and then
				// 2. re-open those that are older than 21 days.
				if time.Since(reopenedAt).Minutes() > 60 {
					for _, order := range open {
						side := bittrexOrderSide(&order)
						if side != model.ORDER_SIDE_NONE {
							var openedAt time.Time
							if openedAt, err = time.Parse(exchange.TIME_FORMAT, order.CreatedAt); err != nil {
								bittrexLogErrEx(errors.Wrap(err, 1), &order, level, service)
							} else {
								if time.Since(openedAt).Hours() >= float64(reopenAfterDays*24) {
									msg := fmt.Sprintf(
										"Cancelling (and reopening) limit %s %s (market: %s, price: %g, qty: %f, opened at %s) because it is older than %d days.",
										model.OrderSideString[side], order.Id, order.MarketName(), order.Price(), order.Quantity, order.CreatedAt, reopenAfterDays,
									)
									log.Println("[INFO] " + msg)
									if service != nil {
										if notify.CanSend(level, notify.INFO) {
											service.SendMessage(msg, "Bittrex - INFO")
										}
									}

									var ocoTriggerPrice float64
									if ocoTriggerPrice, err = bittrexCancelOrder(client, &order); err != nil {
										bittrexLogErrEx(errors.Wrap(err, 1), &order, level, service)
									}

									if ocoTriggerPrice > 0 {
										_, err = self.OCO(client, order.MarketName(), order.Quantity, order.Price(), ocoTriggerPrice, "", "")
									} else {
										_, _, err = self.Order(client, side, order.MarketName(), order.Quantity, order.Price(), model.LIMIT, "")
									}

									if err != nil {
										bittrexLogErrEx(errors.Wrap(err, 1), &order, level, service)
									}
								}
							}
						}
					}
					reopenedAt = time.Now()
				}
			}
		}
	}
}

func (self *Bittrex) Order(
	client interface{},
	side model.OrderSide,
	market1 string,
	size float64,
	price float64,
	kind model.OrderType,
	meta string,
) (oid []byte, raw []byte, err error) {
	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return nil, nil, errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return nil, nil, err
	}

	var order *exchange.Order
	if side == model.BUY {
		if kind == model.MARKET {
			order, err = bittrex.CreateOrder(market3, exchange.BUY, exchange.MARKET, size, 0, exchange.IOC)
		} else if kind == model.LIMIT {
			order, err = bittrex.CreateOrder(market3, exchange.BUY, exchange.LIMIT, size, price, exchange.GTC)
		}
	} else if side == model.SELL {
		if kind == model.MARKET {
			order, err = bittrex.CreateOrder(market3, exchange.SELL, exchange.MARKET, size, 0, exchange.IOC)
		} else if kind == model.LIMIT {
			order, err = bittrex.CreateOrder(market3, exchange.SELL, exchange.LIMIT, size, price, exchange.GTC)
		}
	}

	if err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}

	var out []byte
	if out, err = json.Marshal(order); err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}

	return []byte(order.Id), out, nil
}

func (self *Bittrex) StopLoss(client interface{}, market string, size float64, price float64, kind model.OrderType, meta string) ([]byte, error) {
	return nil, errors.New("Not implemented")
}

func (self *Bittrex) OCO(client interface{}, market1 string, size float64, price, stop float64, meta1, meta2 string) ([]byte, error) {
	var (
		err error
		id  []byte
	)

	if id, _, err = self.Order(client, model.SELL, market1, size, price, model.LIMIT, meta2); err != nil {
		return nil, err
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return nil, err
	}

	var conditionalOrder *exchange.ConditionalOrder
	if conditionalOrder, err = client.(*exchange.Client).CreateConditionalOrder(market3, exchange.LTE, stop, &exchange.NewOrder{
		MarketSymbol: market3,
		Direction:    exchange.SELL,
		OrderType:    exchange.MARKET,
		Quantity:     size,
		Limit:        0,
		TimeInForce:  exchange.IOC,
	}, exchange.OrderId(id)); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	var out []byte
	if out, err = json.Marshal(conditionalOrder); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	return out, nil
}

func (self *Bittrex) GetClosed(client interface{}, market1 string) (model.Orders, error) {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return nil, err
	}

	var history exchange.Orders
	if history, err = bittrex.GetOrderHistory(market3); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	var out model.Orders
	for _, order := range history {
		var closedAt time.Time
		if closedAt, err = time.Parse(exchange.TIME_FORMAT, order.ClosedAt); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		out = append(out, model.Order{
			Side:      bittrexOrderSide(&order),
			Market:    market1,
			Size:      order.Quantity,
			Price:     order.Price(),
			CreatedAt: closedAt,
		})
	}

	return out, nil
}

func (self *Bittrex) GetOpened(client interface{}, market1 string) (model.Orders, error) {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return nil, err
	}

	var orders exchange.Orders
	if orders, err = bittrex.GetOpenOrders(market3); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	var out model.Orders
	for _, order := range orders {
		var openedAt time.Time
		if openedAt, err = time.Parse(exchange.TIME_FORMAT, order.CreatedAt); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		out = append(out, model.Order{
			Side:      bittrexOrderSide(&order),
			Market:    market1,
			Size:      order.Quantity,
			Price:     order.Price(),
			CreatedAt: openedAt,
		})
	}

	return out, nil
}

func (self *Bittrex) GetBook(client interface{}, market1 string, side model.BookSide) (interface{}, error) {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return 0, err
	}

	var book *exchange.OrderBook
	if book, err = bittrex.GetOrderBook(market3, 500); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	switch side {
	case model.BOOK_SIDE_BIDS:
		return book.Bid, nil
	case model.BOOK_SIDE_ASKS:
		return book.Ask, nil
	}

	return nil, errors.Errorf("Non-exhaustive match: %v", side)
}

func (self *Bittrex) Aggregate(client, book interface{}, market string, agg float64) (model.Book, error) {
	bids, ok := book.([]exchange.BookEntry)
	if !ok {
		return nil, errors.New("arg is not a valid v3 order book")
	}

	prec, err := self.GetPricePrec(client, market)
	if err != nil {
		return nil, err
	}

	var out model.Book
	for _, e := range bids {
		price := pricing.RoundToPrecision(pricing.RoundToNearest(e.Rate, agg), prec)
		entry := out.EntryByPrice(price)
		if entry != nil {
			entry.Size = entry.Size + e.Quantity
		} else {
			entry := &model.BookEntry{
				Buy: &model.Buy{
					Market: market,
					Price:  price,
				},
				Size: e.Quantity,
			}
			out = append(out, *entry)
		}
	}

	return out, nil
}

func (self *Bittrex) GetTicker(client interface{}, market1 string) (float64, error) {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return 0, errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return 0, err
	}

	var ticker *exchange.Ticker
	if ticker, err = bittrex.GetTicker(market3); err != nil {
		return 0, errors.Wrap(err, 1)
	}

	return ticker.LastTradeRate, nil
}

func (self *Bittrex) Get24h(client interface{}, market1 string) (*model.Stats, error) {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return nil, err
	}

	var sum *exchange.MarketSummary
	if sum, err = bittrex.GetMarketSummary(market3); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	return &model.Stats{
		Market:    market1,
		High:      sum.High,
		Low:       sum.Low,
		BtcVolume: sum.QuoteVolume,
	}, nil
}

func (self *Bittrex) GetPricePrec(client interface{}, market string) (int, error) {
	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return 0, errors.New("arg is not a valid v3 client")
	}

	if self.markets == nil {
		var err error
		if self.markets, err = bittrex.GetMarkets(); err != nil {
			return 0, errors.Wrap(err, 1)
		}
	}

	for _, market3 := range self.markets {
		if market3.MarketName() == market {
			return market3.Precision, nil
		}
	}

	return 8, nil
}

func (self *Bittrex) GetSizePrec(client interface{}, market string) (int, error) {
	return 8, nil
}

func (self *Bittrex) GetMaxSize(client interface{}, base, quote string, hold bool, def float64) float64 {
	fn := func() int {
		prec, err := self.GetSizePrec(client, self.FormatMarket(base, quote))
		if err != nil {
			return 8
		} else {
			return prec
		}
	}
	return model.GetSizeMax(hold, def, fn)
}

func (self *Bittrex) Cancel(client interface{}, market1 string, side model.OrderSide) error {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return err
	}

	var orders exchange.Orders
	if orders, err = bittrex.GetOpenOrders(market3); err != nil {
		return errors.Wrap(err, 1)
	}

	for _, order := range orders {
		if bittrexOrderSide(&order) == side {
			if err = bittrex.CancelOrder(order.Id); err != nil {
				return errors.Wrap(err, 1)
			}
		}
	}

	return nil
}

func (self *Bittrex) Buy(client interface{}, cancel bool, market1 string, calls model.Calls, size, deviation float64, kind model.OrderType) error {
	var err error

	bittrex, ok := client.(*exchange.Client)
	if !ok {
		return errors.New("arg is not a valid v3 client")
	}

	var market3 string
	if market3, err = self.convertMarket(market1); err != nil {
		return err
	}

	// step #1: delete the buy order(s) that are open in your book
	if cancel {
		var orders exchange.Orders
		if orders, err = bittrex.GetOpenOrders(market3); err != nil {
			return errors.Wrap(err, 1)
		}
		for _, order := range orders {
			side := bittrexOrderSide(&order)
			if side == model.BUY {
				// do not cancel orders that we're about to re-place
				index := calls.IndexByPrice(order.Price())
				if index > -1 && order.Quantity == size {
					calls[index].Skip = true
				} else {
					if err = bittrex.CancelOrder(order.Id); err != nil {
						return errors.Wrap(err, 1)
					}
				}
			}
		}
	}

	// step 2: open the top X buy orders
	for _, call := range calls {
		if !call.Skip {
			limit := call.Price
			if deviation != 1.0 {
				kind, limit = call.Deviate(self, client, kind, deviation)
			}
			_, _, err = self.Order(client,
				model.BUY,
				market1,
				size,
				limit,
				kind, "",
			)
			if err != nil {
				// --- BEGIN --- svanas 2019-05-12 ------------------------------------
				if strings.Contains(err.Error(), "MIN_TRADE_REQUIREMENT_NOT_MET") {
					var markets []exchange.Market
					if markets, err = bittrex.GetMarkets(); err != nil {
						return errors.Wrap(err, 1)
					}
					for m := range markets {
						if markets[m].MarketName() == market1 {
							return self.Buy(client, cancel, market1, calls, markets[m].MinTradeSize, deviation, kind)
						}
					}
				}
				// ---- END ---- svanas 2019-05-12 ------------------------------------
				return err
			}
		}
	}

	return nil
}

func (self *Bittrex) IsLeveragedToken(name string) bool {
	return len(name) > 4 && (strings.HasSuffix(strings.ToUpper(name), "BEAR") || strings.HasSuffix(strings.ToUpper(name), "BULL"))
}

func NewBittrex() model.Exchange {
	return &Bittrex{
		ExchangeInfo: &model.ExchangeInfo{
			Code: "BTRX",
			Name: "Bittrex",
			URL:  "https://bittrex.com",
			REST: model.Endpoint{
				URI: "https://api.bittrex.com/v3",
			},
			Country: "USA",
		},
	}
}
