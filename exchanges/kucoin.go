package exchanges

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"
	"time"

	exchange "github.com/Kucoin/kucoin-go-sdk"
	filemutex "github.com/alexflint/go-filemutex"
	"github.com/go-errors/errors"
	"github.com/svanas/nefertiti/flag"
	"github.com/svanas/nefertiti/model"
	"github.com/svanas/nefertiti/notify"
	"github.com/svanas/nefertiti/pricing"
	"github.com/svanas/nefertiti/session"
	"github.com/svanas/nefertiti/uuid"
)

var (
	kucoinMutex *filemutex.FileMutex
)

const (
	kucoinSessionFile = "kucoin.time"
	kucoinSessionLock = "kucoin.lock"
)

func init() {
	exchange.BeforeRequest = func(client *exchange.ApiService, request *exchange.Request) error {
		var err error

		if kucoinMutex == nil {
			if kucoinMutex, err = filemutex.New(session.GetSessionFile(kucoinSessionLock)); err != nil {
				return err
			}
		}

		if err = kucoinMutex.Lock(); err != nil {
			return err
		}

		var lastRequest *time.Time
		if lastRequest, err = session.GetLastRequest(kucoinSessionFile); err != nil {
			return err
		}

		if lastRequest != nil {
			elapsed := time.Since(*lastRequest)
			rps := exchange.RequestsPerSecond(request)
			if elapsed.Seconds() < (float64(1) / rps) {
				sleep := time.Duration((float64(time.Second) / rps)) - elapsed
				if flag.Debug() {
					log.Printf("[DEBUG] sleeping %f seconds", sleep.Seconds())
				}
				time.Sleep(sleep)
			}
		}

		if flag.Debug() {
			log.Printf("[DEBUG] %s %s", request.Method, request.Path)
		}

		return nil
	}
	exchange.AfterRequest = func() {
		defer func() {
			kucoinMutex.Unlock()
		}()
		session.SetLastRequest(kucoinSessionFile, time.Now())
	}
}

type Kucoin struct {
	*model.ExchangeInfo
	symbols exchange.SymbolsModel
}

//-------------------- private -------------------

func (self *Kucoin) baseURI(sandbox bool) string {
	if sandbox {
		return self.ExchangeInfo.REST.Sandbox
	} else {
		return self.ExchangeInfo.REST.URI
	}
}

func (self *Kucoin) error(err error, level int64, service model.Notify) {
	pc, file, line, _ := runtime.Caller(1)
	prefix := errors.FormatCaller(pc, file, line)

	msg := fmt.Sprintf("%s %v", prefix, err)

	if strings.Contains(err.Error(), "no such host") {
		log.Printf("[ERROR] %s", msg)
		return
	}

	_, ok := err.(*errors.Error)
	if ok && flag.Debug() {
		log.Printf("[ERROR] %s", err.(*errors.Error).ErrorStack(prefix, ""))
	} else {
		log.Printf("[ERROR] %s", msg)
	}

	if service != nil {
		if notify.CanSend(level, notify.ERROR) {
			err := service.SendMessage(msg, "Kucoin - ERROR")
			if err != nil {
				log.Printf("[ERROR] %v", err)
			}
		}
	}
}

// func (self *Kucoin) getAvailableBalance(client *exchange.ApiService, curr string) (float64, error) {
// 	var (
// 		err      error
// 		out      float64
// 		resp     *exchange.ApiResponse
// 		accounts exchange.AccountsModel
// 	)
// 	if resp, err = client.Accounts(curr, "trade"); err != nil {
// 		return 0, errors.Wrap(err, 1)
// 	}
// 	if err = resp.ReadData(&accounts); err != nil {
// 		return 0, errors.Wrap(err, 1)
// 	}
// 	if len(accounts) == 0 {
// 		return 0, errors.Errorf("Currency %s does not exist", curr)
// 	}
// 	if out, err = strconv.ParseFloat(accounts[0].Available, 64); err != nil {
// 		return 0, errors.Wrap(err, 1)
// 	}
// 	return out, nil
// }

func (self *Kucoin) getSymbols(client *exchange.ApiService, cached bool) (exchange.SymbolsModel, error) {
	if self.symbols == nil || cached == false {
		var (
			err     error
			resp    *exchange.ApiResponse
			symbols exchange.SymbolsModel
		)
		if resp, err = client.Symbols(""); err != nil {
			return self.symbols, errors.Wrap(err, 1)
		}
		if err = resp.ReadData(&symbols); err != nil {
			return self.symbols, errors.Wrap(err, 1)
		}
		self.symbols = symbols
	}
	return self.symbols, nil
}

func (self *Kucoin) getMinSize(client *exchange.ApiService, market string, cached bool) (float64, error) {
	symbols, err := self.getSymbols(client, cached)
	if err != nil {
		return 0, err
	}
	for _, symbol := range symbols {
		if symbol.Symbol == market {
			out, err := strconv.ParseFloat(symbol.BaseMinSize, 64)
			if err != nil {
				return 0, errors.Wrap(err, 1)
			}
			return out, nil
		}
	}
	return 0, nil
}

// getOrders returns a list your current orders.
func (self *Kucoin) getOrders(client *exchange.ApiService, params map[string]string) (exchange.OrdersModel, error) {
	var (
		err    error
		curr   int64
		output exchange.OrdersModel
	)

	curr = 1
	for true {
		var resp *exchange.ApiResponse
		if resp, err = client.Orders(params, &exchange.PaginationParam{CurrentPage: curr, PageSize: 50}); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		var (
			page   *exchange.PaginationModel
			orders exchange.OrdersModel
		)
		if page, err = resp.ReadPaginationData(&orders); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		output = append(output, orders...)
		if page.CurrentPage >= page.TotalPage {
			break
		} else {
			curr++
		}
	}

	curr = 1
	for true {
		var resp *exchange.ApiResponse
		if resp, err = client.StopOrders(params, &exchange.PaginationParam{CurrentPage: curr, PageSize: 50}); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		var (
			page   *exchange.PaginationModel
			orders exchange.OrdersModel
		)
		if page, err = resp.ReadPaginationData(&orders); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		output = append(output, orders...)
		if page.CurrentPage >= page.TotalPage {
			break
		} else {
			curr++
		}
	}

	return output, nil
}

// getRecentFills returns a list of your recent fills, up to max orders.
func (self *Kucoin) getRecentFills(client *exchange.ApiService, max int) (exchange.FillsModel, error) {
	const PAGE_SIZE = 100

	var (
		err    error
		curr   int64 = 1
		output exchange.FillsModel
	)

	for {
		var resp *exchange.ApiResponse
		if resp, err = client.Fills(map[string]string{}, &exchange.PaginationParam{CurrentPage: curr, PageSize: PAGE_SIZE}); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		var (
			page   *exchange.PaginationModel
			orders exchange.FillsModel
		)
		if page, err = resp.ReadPaginationData(&orders); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		if len(orders) == 0 {
			if page.CurrentPage == 1 && page.TotalPage > 1 || page.CurrentPage > 1 {
				errors.Errorf("/api/v1/fills?currentPage=%d&pageSize=%d returned 0 orders, expected at least 1.", curr, PAGE_SIZE)
			}
		}
		output = append(output, orders...)
		if len(output) >= max || page.CurrentPage >= page.TotalPage {
			break
		} else {
			curr++
		}
	}

	return output, nil
}

//-------------------- public --------------------

func (self *Kucoin) GetInfo() *model.ExchangeInfo {
	return self.ExchangeInfo
}

func (self *Kucoin) GetClient(permission model.Permission, sandbox bool) (interface{}, error) {
	// starting 04/26/21, the KuCoin order book endpoints require authentication.
	if permission == model.PUBLIC {
		return exchange.NewApiService(
			exchange.ApiBaseURIOption(self.baseURI(sandbox)),
		), nil
	}

	apiKey, apiSecret, apiPassphrase, err := promptForApiKeysEx("KuCoin")
	if err != nil {
		return nil, err
	}

	return exchange.NewApiService(
		exchange.ApiBaseURIOption(self.baseURI(sandbox)),
		exchange.ApiKeyOption(apiKey),
		exchange.ApiSecretOption(apiSecret),
		exchange.ApiPassPhraseOption(apiPassphrase),
	), nil
}

func (self *Kucoin) GetMarkets(cached, sandbox bool) ([]model.Market, error) {
	var out []model.Market

	client := exchange.NewApiService(
		exchange.ApiBaseURIOption(self.baseURI(sandbox)),
	)

	symbols, err := self.getSymbols(client, cached)
	if err != nil {
		return nil, err
	}

	for _, symbol := range symbols {
		if symbol.EnableTrading {
			out = append(out, model.Market{
				Name:  symbol.Symbol,
				Base:  symbol.BaseCurrency,
				Quote: symbol.QuoteCurrency,
			})
		}
	}

	return out, nil
}

func (self *Kucoin) FormatMarket(base, quote string) string {
	return strings.ToUpper(fmt.Sprintf("%s-%s", base, quote))
}

// listens to the filled orders, look for newly filled orders, automatically place new sell orders.
func (self *Kucoin) sell(
	client *exchange.ApiService,
	strategy model.Strategy,
	mult float64,
	hold model.Markets,
	service model.Notify,
	twitter *notify.TwitterKeys,
	level int64,
	old exchange.FillsModel,
	sandbox bool,
	debug bool,
) (exchange.FillsModel, error) {
	var err error

	// get my filled orders
	var new exchange.FillsModel
	if new, err = self.getRecentFills(client, 500); err != nil {
		return old, err
	}

	if len(old) > 0 {
		if len(new) > 0 {
			for _, order := range old {
				if new.IndexOfOrderId(order.OrderId) > -1 {
					goto WeAreGood
				}
			}
		}
		goto WhatTheFuck
	WhatTheFuck:
		return old, errors.Errorf("/api/v1/fills returned %d orders, expected at least %d.", len(new), len(old))
	WeAreGood:
		// nothing to see here, carry on
	}

	// get the markets
	var markets []model.Market
	if markets, err = self.GetMarkets(false, sandbox); err != nil {
		return old, err
	}

	type (
		SimpleOrder struct {
			Low  float64
			High float64
			Size float64
		}
		SimpleOrders map[string]*SimpleOrder
	)
	avg := func(order *SimpleOrder) float64 {
		if order.Low == 0 && order.High > 0 {
			return order.High
		}
		if order.High == 0 && order.Low > 0 {
			return order.Low
		}
		if order.High == 0 && order.Low == 0 {
			return 0
		} else {
			return (order.Low + order.High) / 2
		}
	}
	var (
		bought  = make(SimpleOrders)
		stopped = make(SimpleOrders)
	)

	// look for newly filled orders
	for _, order := range new {
		if old.IndexOfOrderId(order.OrderId) == -1 {
			side := model.NewOrderSide(order.Side)

			var data []byte
			if data, err = json.Marshal(order); err != nil {
				return new, errors.Wrap(err, 1)
			}
			log.Println("[FILLED] " + string(data))

			var orders SimpleOrders = nil
			if side == model.BUY {
				orders = bought
			} else if side == model.SELL && order.Stop == "loss" {
				orders = stopped
			}

			if orders != nil {
				o, exist := orders[order.Symbol]
				if !exist {
					orders[order.Symbol] = &SimpleOrder{
						Low:  order.ParsePrice(),
						High: order.ParsePrice(),
						Size: order.ParseSize(),
					}
				} else {
					// add up amount(s), hereby preventing a problem with partial matches
					price := order.ParsePrice()
					if price > 0 {
						if price < o.Low {
							orders[order.Symbol].Low = price
						} else if price > o.High {
							orders[order.Symbol].High = price
						}
					}
					orders[order.Symbol].Size = o.Size + order.ParseSize()
				}
			}

			// send notification(s)
			if side != model.ORDER_SIDE_NONE {
				if notify.CanSend(level, notify.FILLED) {
					if service != nil {
						title := fmt.Sprintf("Kucoin - Done %s", model.FormatOrderSide(side))
						if side == model.SELL {
							if strategy == model.STRATEGY_STOP_LOSS {
								perc := (mult - 1) * 100
								if order.Stop == "loss" {
									perc = perc * -2
								}
								title = fmt.Sprintf("%s %.2f%%", title, perc)
							}
						}
						if err = service.SendMessage(string(data), title); err != nil {
							log.Printf("[ERROR] %v", err)
						}
					}
					if twitter != nil {
						notify.Tweet(twitter, fmt.Sprintf("Done %s. %s priced at %s #Kucoin", model.FormatOrderSide(side), model.TweetMarket(markets, order.Symbol), order.Price))
					}
				}
			}
		}
	}

	// has a stop loss been filled? then place a buy order double the order size
	if strategy == model.STRATEGY_STOP_LOSS {
		for symbol, stop := range stopped {
			var ticker float64
			if ticker, err = self.GetTicker(client, symbol); err != nil {
				return new, err
			}
			if ticker > stop.High {
				log.Printf("[INFO] Not re-buying %s because ticker %.8f is higher than stop price %.8f\n", symbol, ticker, stop.High)
			} else {
				var opened exchange.OrdersModel
				if opened, err = self.getOrders(client, map[string]string{"status": "active", "symbol": symbol}); err != nil {
					self.error(err, level, service)
				} else {
					var cb exchange.OrderPredicate
					cb = func(order *exchange.OrderModel) bool {
						return order.Stop == "loss" && order.Side == "sell"
					}
					if opened.Find(&cb) > -1 {
						log.Printf("[INFO] Not re-buying %s because you have at least one active (non-filled) stop-loss order.\n", symbol)
					} else {
						prec := 0
						size := 2 * stop.Size
						if prec, err = self.GetSizePrec(client, symbol); err == nil {
							_, _, err = self.Order(client,
								model.BUY, symbol,
								pricing.RoundToPrecision(size, prec),
								0, model.MARKET, "",
							)
						}
					}
				}
			}
		}
	}

	// has a buy order been filled? then place a sell order
	for symbol, buy := range bought {
		amount := buy.Size
		bought := avg(buy)

		var sp int
		if sp, err = self.GetSizePrec(client, symbol); err != nil {
			return new, err
		} else {
			amount = pricing.FloorToPrecision(amount, sp)
		}

		if bought == 0 {
			if bought, err = self.GetTicker(client, symbol); err != nil {
				return new, err
			}
		}

		// get base currency and desired size, calculate price, place sell order
		var (
			base  string
			quote string
		)
		base, quote, err = model.ParseMarket(markets, symbol)
		if err == nil {
			// --- BEGIN --- svanas 2019-02-19 --- if we have dust, try and sell it ---
			// var available float64
			// if available, err = self.getAvailableBalance(client, base); err != nil {
			// 	self.error(err, level, service)
			// } else {
			// 	available = pricing.FloorToPrecision(available, sp)
			// 	if available > amount {
			// 		amount = available
			// 	}
			// }
			// ---- END ---- svanas 2019-02-19 ----------------------------------------
			var pp int
			if pp, err = self.GetPricePrec(client, symbol); err == nil {
				if strategy == model.STRATEGY_TRAILING_STOP_LOSS || strategy == model.STRATEGY_STOP_LOSS {
					var ticker float64
					if ticker, err = self.GetTicker(client, symbol); err == nil {
						sold := false
						if strategy == model.STRATEGY_STOP_LOSS {
							if ticker >= pricing.Multiply(bought, mult, pp) {
								_, _, err = self.Order(client,
									model.SELL,
									symbol,
									amount,
									0, model.MARKET, "",
								)
								sold = true
							}
						}
						if !sold {
							var stop float64
							if strategy == model.STRATEGY_STOP_LOSS {
								stop = ticker / pricing.NewMult(mult, 2.0)
							} else {
								stop = ticker / pricing.NewMult(mult, 0.5)
							}
							_, err = self.StopLoss(client,
								symbol,
								amount,
								pricing.RoundToPrecision(stop, pp),
								model.MARKET, "",
							)
						}
					}
				} else {
					amount = self.GetMaxSize(client, base, quote, hold.HasMarket(symbol), amount)
					if amount > 0 {
						_, _, err = self.Order(client,
							model.SELL,
							symbol,
							amount,
							pricing.Multiply(bought, mult, pp),
							model.LIMIT, "",
						)
					}
				}
			}
		}

		if err != nil {
			var data []byte
			if data, _ = json.Marshal(buy); data == nil {
				self.error(err, level, service)
			} else {
				self.error(errors.Append(err, "\t", string(data)), level, service)
			}
		}
	}

	return new, nil
}

// listens to the open orders, look for cancelled orders, send a notification on newly opened orders.
func (self *Kucoin) listen(
	client *exchange.ApiService,
	service model.Notify,
	level int64,
	old exchange.OrdersModel,
	filled exchange.FillsModel,
) (exchange.OrdersModel, error) {
	var (
		err error
		new exchange.OrdersModel
	)

	// get my opened orders
	if new, err = self.getOrders(client, map[string]string{"status": "active"}); err != nil {
		return old, err
	}

	// look for cancelled orders
	for _, order := range old {
		if new.IndexOfId(order.Id) == -1 {
			// if this order has NOT been FILLED, then it has been cancelled.
			if filled.IndexOfOrderId(order.Id) == -1 {
				var data []byte
				if data, err = json.Marshal(order); err != nil {
					return new, errors.Wrap(err, 1)
				}

				log.Println("[CANCELLED] " + string(data))

				side := model.NewOrderSide(order.Side)
				if side != model.ORDER_SIDE_NONE {
					if service != nil && notify.CanSend(level, notify.CANCELLED) {
						if err = service.SendMessage(string(data), fmt.Sprintf("Kucoin - Cancelled %s", model.FormatOrderSide(side))); err != nil {
							log.Printf("[ERROR] %v", err)
						}
					}
				}
			}
		}
	}

	// look for new orders
	for _, order := range new {
		if old.IndexOfId(order.Id) == -1 {
			var data []byte
			if data, err = json.Marshal(order); err != nil {
				return new, errors.Wrap(err, 1)
			}

			log.Println("[OPEN] " + string(data))

			if service != nil {
				side := model.NewOrderSide(order.Side)
				if side != model.ORDER_SIDE_NONE {
					if notify.CanSend(level, notify.OPENED) || (level == notify.LEVEL_DEFAULT && side == model.SELL) {
						if err = service.SendMessage(string(data), ("Kucoin - Open " + model.FormatOrderSide(side))); err != nil {
							log.Printf("[ERROR] %v", err)
						}
					}
				}
			}
		}
	}

	return new, nil
}

func (self *Kucoin) Sell(
	start time.Time,
	hold model.Markets,
	sandbox, tweet, debug bool,
	success model.OnSuccess,
) error {
	var err error

	strategy := model.GetStrategy()
	if strategy == model.STRATEGY_STANDARD || strategy == model.STRATEGY_TRAILING || strategy == model.STRATEGY_TRAILING_STOP_LOSS || strategy == model.STRATEGY_STOP_LOSS {
		// we are OK
	} else {
		return errors.New("Strategy not implemented")
	}

	var (
		apiKey        string
		apiSecret     string
		apiPassphrase string
	)
	if apiKey, apiSecret, apiPassphrase, err = promptForApiKeysEx("KuCoin"); err != nil {
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

	client := exchange.NewApiService(
		exchange.ApiBaseURIOption(self.baseURI(sandbox)),
		exchange.ApiKeyOption(apiKey),
		exchange.ApiSecretOption(apiSecret),
		exchange.ApiPassPhraseOption(apiPassphrase),
	)

	// get my filled orders
	var filled exchange.FillsModel
	if filled, err = self.getRecentFills(client, 500); err != nil {
		return err
	}

	// get my opened orders
	var opened exchange.OrdersModel
	if opened, err = self.getOrders(client, map[string]string{"status": "active"}); err != nil {
		return err
	}

	if err = success(service); err != nil {
		return err
	}

	for {
		// read the dynamic settings
		var (
			level    int64          = notify.Level()
			mult     float64        = model.GetMult()
			strategy model.Strategy = model.GetStrategy()
		)
		// listens to the filled orders, look for newly filled orders, automatically place new sell orders.
		filled, err = self.sell(client, strategy, mult, hold, service, twitter, level, filled, sandbox, debug)
		if err != nil {
			self.error(err, level, service)
			time.Sleep(time.Minute)
		} else {
			// listens to the open orders, look for cancelled orders, send a notification on newly opened orders.
			opened, err = self.listen(client, service, level, opened, filled)
			if err != nil {
				self.error(err, level, service)
				time.Sleep(time.Minute)
			} else {
				// listens to the open orders, follow up on the trailing stop loss strategy
				if strategy == model.STRATEGY_TRAILING || strategy == model.STRATEGY_TRAILING_STOP_LOSS || strategy == model.STRATEGY_STOP_LOSS {
					cache := make(map[string]float64)
					for _, order := range opened {
						// enumerate over stop loss orders
						if order.Stop == "loss" {
							ticker, ok := cache[order.Symbol]
							if !ok {
								if ticker, err = self.GetTicker(client, order.Symbol); err == nil {
									cache[order.Symbol] = ticker
								}
							}
							if ticker > 0 {
								var prec int
								if prec, err = self.GetPricePrec(client, order.Symbol); err == nil {
									sold := false
									if strategy == model.STRATEGY_STOP_LOSS {
										bought := pricing.Multiply(order.ParseStopPrice(), pricing.NewMult(mult, 2.0), prec)
										if ticker >= pricing.Multiply(bought, mult, prec) {
											if _, err = client.CancelOrder(order.Id); err == nil {
												_, _, err = self.Order(client,
													model.SELL,
													order.Symbol,
													order.ParseSize(),
													0, model.MARKET, "",
												)
												sold = true
											}
										}
									}
									if !sold {
										if strategy != model.STRATEGY_STOP_LOSS {
											var price float64
											price = pricing.NewMult(mult, 0.5) * (ticker / mult)
											// is the distance bigger than 5%? then cancel the stop loss, and place a new one.
											if order.ParseStopPrice() < pricing.RoundToPrecision(price, prec) {
												if _, err = client.CancelOrder(order.Id); err == nil {
													_, err = self.StopLoss(client,
														order.Symbol,
														order.ParseSize(),
														pricing.RoundToPrecision(price, prec),
														model.NewOrderType(order.Type), "",
													)
												}
											}
										}
									}
								}
								if err != nil {
									var data []byte
									if data, _ = json.Marshal(order); data == nil {
										self.error(err, level, service)
									} else {
										self.error(errors.Append(err, "\t", string(data)), level, service)
									}
								}
							}
						}
						// enumerate over limit sell orders
						if order.Stop == "" {
							side := model.NewOrderSide(order.Side)
							if side == model.SELL {
								if strategy != model.STRATEGY_STOP_LOSS {
									ticker, ok := cache[order.Symbol]
									if !ok {
										if ticker, err = self.GetTicker(client, order.Symbol); err == nil {
											cache[order.Symbol] = ticker
										}
									}
									if ticker > 0 {
										var price float64
										price = pricing.NewMult(mult, 0.75) * (order.ParsePrice() / mult)
										// is the ticker nearing the price? then cancel the limit sell order, and place a stop loss order below the ticker.
										if ticker > price {
											if _, err = client.CancelOrder(order.Id); err == nil {
												var prec int
												if prec, err = self.GetPricePrec(client, order.Symbol); err == nil {
													price = pricing.NewMult(mult, 0.5) * (ticker / mult)
													_, err = self.StopLoss(client,
														order.Symbol,
														order.ParseSize(),
														pricing.RoundToPrecision(price, prec),
														model.MARKET, "",
													)
												}
											}
											if err != nil {
												var data []byte
												if data, _ = json.Marshal(order); data == nil {
													self.error(err, level, service)
												} else {
													self.error(errors.Append(err, "\t", string(data)), level, service)
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func (self *Kucoin) Order(
	client interface{},
	side model.OrderSide,
	market string,
	size float64,
	price float64,
	kind model.OrderType,
	meta string,
) (oid []byte, raw []byte, err error) {
	var (
		resp  *exchange.ApiResponse
		order exchange.CreateOrderResultModel
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return nil, nil, errors.New("invalid argument: client")
	}

	var params = map[string]string{
		"clientOid": uuid.New().Long(),
		"side":      side.String(),
		"symbol":    market,
		"type":      kind.String(),
		"size":      strconv.FormatFloat(size, 'f', -1, 64),
	}
	if kind == model.LIMIT {
		params["price"] = strconv.FormatFloat(price, 'f', -1, 64)
	}

	if resp, err = kucoin.CreateOrder(params); err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}
	if err = resp.ReadData(&order); err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}
	if raw, err = json.Marshal(order); err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}

	return []byte(order.OrderId), raw, nil
}

func (self *Kucoin) StopLoss(client interface{}, market string, size float64, price float64, kind model.OrderType, meta string) ([]byte, error) {
	var (
		err   error
		out   []byte
		resp  *exchange.ApiResponse
		order exchange.CreateOrderResultModel
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	var params = map[string]string{
		"clientOid": uuid.New().Long(),
		"side":      model.OrderSideString[model.SELL],
		"symbol":    market,
		"type":      kind.String(),
		"size":      strconv.FormatFloat(size, 'f', -1, 64),
		"stop":      "loss",
		"stopPrice": strconv.FormatFloat(price, 'f', -1, 64),
	}
	if kind == model.LIMIT {
		params["price"] = strconv.FormatFloat(price, 'f', -1, 64)
	}

	if resp, err = kucoin.CreateOrder(params); err != nil {
		return nil, errors.Wrap(err, 1)
	}
	if err = resp.ReadData(&order); err != nil {
		return nil, errors.Wrap(err, 1)
	}
	if out, err = json.Marshal(order); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	return out, nil
}

func (self *Kucoin) OCO(client interface{}, market string, size float64, price, stop float64, meta1, meta2 string) ([]byte, error) {
	return nil, errors.New("Not implemented")
}

func (self *Kucoin) GetClosed(client interface{}, market string) (model.Orders, error) {
	var (
		err   error
		resp  *exchange.ApiResponse
		page  *exchange.PaginationModel
		fills exchange.FillsModel
		out   model.Orders
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	var curr int64 = 1
	for true {
		if resp, err = kucoin.Fills(
			map[string]string{"symbol": market},
			&exchange.PaginationParam{CurrentPage: curr, PageSize: 50}); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		if page, err = resp.ReadPaginationData(&fills); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		for _, fill := range fills {
			out = append(out, model.Order{
				Side:      model.NewOrderSide(fill.Side),
				Market:    fill.Symbol,
				Size:      fill.ParseSize(),
				Price:     fill.ParsePrice(),
				CreatedAt: fill.ParseCreatedAt(),
			})
		}
		if page.CurrentPage >= page.TotalPage {
			break
		} else {
			curr++
		}
	}

	return out, nil
}

func (self *Kucoin) GetOpened(client interface{}, market string) (model.Orders, error) {
	var (
		err    error
		out    model.Orders
		orders exchange.OrdersModel
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	if orders, err = self.getOrders(kucoin, map[string]string{"status": "active", "symbol": market}); err != nil {
		return nil, err
	}

	for _, order := range orders {
		out = append(out, model.Order{
			Side:      model.NewOrderSide(order.Side),
			Market:    order.Symbol,
			Size:      order.ParseSize(),
			Price:     order.ParsePrice(),
			CreatedAt: order.ParseCreatedAt(),
		})
	}

	return out, nil
}

func (self *Kucoin) GetBook(client interface{}, market string, side model.BookSide) (interface{}, error) {
	var (
		err  error
		out  []exchange.BookEntry
		resp *exchange.ApiResponse
		book exchange.FullOrderBookModel
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return 0, errors.New("invalid argument: client")
	}

	if resp, err = kucoin.AggregatedFullOrderBook(market); err != nil {
		return nil, errors.Wrap(err, 1)
	}
	if err = resp.ReadData(&book); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	if side == model.BOOK_SIDE_ASKS {
		out = book.Asks
	} else {
		out = book.Bids
	}

	return out, nil
}

func (self *Kucoin) Aggregate(client, book interface{}, market string, agg float64) (model.Book, error) {
	bids, ok := book.([]exchange.BookEntry)
	if !ok {
		return nil, errors.New("invalid argument: book")
	}

	prec, err := self.GetPricePrec(client, market)
	if err != nil {
		return nil, err
	}

	var out model.Book
	for _, e := range bids {
		price := pricing.RoundToPrecision(pricing.RoundToNearest(e.Price(), agg), prec)
		entry := out.EntryByPrice(price)
		if entry != nil {
			entry.Size = entry.Size + e.Size()
		} else {
			entry = &model.BookEntry{
				Buy: &model.Buy{
					Market: market,
					Price:  price,
				},
				Size: e.Size(),
			}
			out = append(out, *entry)
		}
	}

	return out, nil
}

func (self *Kucoin) GetTicker(client interface{}, market string) (float64, error) {
	var (
		err    error
		out    float64
		resp   *exchange.ApiResponse
		ticker exchange.TickerLevel1Model
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return 0, errors.New("invalid argument: client")
	}

	if resp, err = kucoin.TickerLevel1(market); err != nil {
		return 0, errors.Wrap(err, 1)
	}
	if err = resp.ReadData(&ticker); err != nil {
		return 0, errors.Wrap(err, 1)
	}
	if out, err = strconv.ParseFloat(ticker.Price, 64); err != nil {
		return 0, errors.Wrap(err, 1)
	}

	return out, nil
}

func (self *Kucoin) Get24h(client interface{}, market string) (*model.Stats, error) {
	var (
		err  error
		resp *exchange.ApiResponse
		json exchange.Stats24hrModel
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	if resp, err = kucoin.Stats24hr(market); err != nil {
		return nil, errors.Wrap(err, 1)
	}
	if err = resp.ReadData(&json); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	var high float64
	if high, err = strconv.ParseFloat(json.High, 64); err != nil {
		log.Printf("[ERROR] %s\n%+v\n", errors.Wrap(err, 1).ErrorStack("kucoin.go::Get24h", ""), json)
	}

	var low float64
	if low, err = strconv.ParseFloat(json.Low, 64); err != nil {
		log.Printf("[ERROR] %s\n%+v\n", errors.Wrap(err, 1).ErrorStack("kucoin.go::Get24h", ""), json)
	}

	var volume float64
	if volume, err = strconv.ParseFloat(json.VolValue, 64); err != nil {
		log.Printf("[ERROR] %s\n%+v\n", errors.Wrap(err, 1).ErrorStack("kucoin.go::Get24h", ""), json)
	}

	return &model.Stats{
		Market:    market,
		High:      high,
		Low:       low,
		BtcVolume: volume,
	}, nil
}

func (self *Kucoin) GetPricePrec(client interface{}, market string) (int, error) {
	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return 0, errors.New("invalid argument: client")
	}
	symbols, err := self.getSymbols(kucoin, true)
	if err != nil {
		return 0, err
	}
	for _, symbol := range symbols {
		if symbol.Symbol == market {
			return getPrecFromStr(symbol.PriceIncrement, 8), nil
		}
	}
	return 8, nil
}

func (self *Kucoin) GetSizePrec(client interface{}, market string) (int, error) {
	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return 0, errors.New("invalid argument: client")
	}
	symbols, err := self.getSymbols(kucoin, true)
	if err != nil {
		return 0, err
	}
	for _, symbol := range symbols {
		if symbol.Symbol == market {
			return getPrecFromStr(symbol.BaseIncrement, 0), nil
		}
	}
	return 0, nil
}

func (self *Kucoin) GetMaxSize(client interface{}, base, quote string, hold bool, def float64) float64 {
	if hold {
		if base == "KCS" {
			return 0
		}
	}
	fn := func() int {
		prec, err := self.GetSizePrec(client, self.FormatMarket(base, quote))
		if err != nil {
			return 0
		} else {
			return prec
		}
	}
	return model.GetSizeMax(hold, def, fn)
}

func (self *Kucoin) Cancel(client interface{}, market string, side model.OrderSide) error {
	var (
		err    error
		orders exchange.OrdersModel
	)

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return errors.New("invalid argument: client")
	}

	if orders, err = self.getOrders(kucoin, map[string]string{
		"status": "active",
		"symbol": market,
		"side":   side.String(),
	}); err != nil {
		return err
	}

	for _, order := range orders {
		if _, err = kucoin.CancelOrder(order.Id); err != nil {
			return errors.Wrap(err, 1)
		}
	}

	return nil
}

func (self *Kucoin) Buy(client interface{}, cancel bool, market string, calls model.Calls, size, deviation float64, kind model.OrderType) error {
	var err error

	kucoin, ok := client.(*exchange.ApiService)
	if !ok {
		return errors.New("invalid argument: client")
	}

	// step #1: delete the buy order(s) that are open in your book
	if cancel {
		var orders exchange.OrdersModel
		if orders, err = self.getOrders(kucoin, map[string]string{
			"status": "active",
			"symbol": market,
			"side":   "buy",
			"type":   "limit",
		}); err != nil {
			return err
		}
		for _, order := range orders {
			// do not cancel orders that we're about to re-place
			index := calls.IndexByPrice(order.ParsePrice())
			if index > -1 {
				calls[index].Skip = true
			} else {
				if _, err = kucoin.CancelOrder(order.Id); err != nil {
					return errors.Wrap(err, 1)
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
			qty := call.Quantity
			if qty == 0 {
				qty = size
			}
			// respect the baseMinSize
			var min float64
			if min, err = self.getMinSize(kucoin, market, true); err != nil {
				return err
			}
			if qty < min {
				qty = min
			}
			_, _, err = self.Order(client,
				model.BUY,
				market,
				qty,
				limit,
				kind, "",
			)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (self *Kucoin) IsLeveragedToken(name string) bool {
	return len(name) > 2 && (strings.HasSuffix(strings.ToUpper(name), "3L") || strings.HasSuffix(strings.ToUpper(name), "3S"))
}

func NewKucoin() model.Exchange {
	return &Kucoin{
		ExchangeInfo: &model.ExchangeInfo{
			Code: "KUCN",
			Name: "KuCoin",
			URL:  "https://www.kucoin.com",
			REST: model.Endpoint{
				URI:     "https://api.kucoin.com",
				Sandbox: "https://openapi-sandbox.kucoin.com",
			},
			Country: "Hong Kong",
		},
	}
}
