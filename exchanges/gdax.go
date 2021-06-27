package exchanges

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"
	"time"

	filemutex "github.com/alexflint/go-filemutex"
	"github.com/go-errors/errors"
	ws "github.com/gorilla/websocket"
	exchange "github.com/preichenberger/go-coinbase-exchange"
	"github.com/svanas/nefertiti/flag"
	"github.com/svanas/nefertiti/model"
	"github.com/svanas/nefertiti/notify"
	"github.com/svanas/nefertiti/pricing"
	"github.com/svanas/nefertiti/session"
)

var (
	gdaxMutex *filemutex.FileMutex
)

const (
	gdaxSessionFile = "gdax.time"
	gdaxSessionLock = "gdax.lock"
)

func init() {
	exchange.BeforeRequest = func(client *exchange.Client, method, endpoint string) error {
		var err error

		if gdaxMutex == nil {
			if gdaxMutex, err = filemutex.New(session.GetSessionFile(gdaxSessionLock)); err != nil {
				return err
			}
		}

		if err = gdaxMutex.Lock(); err != nil {
			return err
		}

		var lastRequest *time.Time
		if lastRequest, err = session.GetLastRequest(gdaxSessionFile); err != nil {
			return err
		}

		if lastRequest != nil {
			elapsed := time.Since(*lastRequest)
			if elapsed.Seconds() < (float64(1) / exchange.RequestsPerSecond) {
				sleep := time.Duration((float64(time.Second) / exchange.RequestsPerSecond)) - elapsed
				if flag.Debug() {
					log.Printf("[DEBUG] sleeping %f seconds", sleep.Seconds())
				}
				time.Sleep(sleep)
			}
		}

		if flag.Debug() {
			log.Printf("[DEBUG] %s %s", method, endpoint)
		}

		return nil
	}
	exchange.AfterRequest = func() {
		defer func() {
			gdaxMutex.Unlock()
		}()
		session.SetLastRequest(gdaxSessionFile, time.Now())
	}
}

type GdaxMsgType int

const (
	GDAX_MSG_UNKNOWN GdaxMsgType = iota
	GDAX_MSG_RECEIVED
	GDAX_MSG_OPEN
	GDAX_MSG_DONE
	GDAX_MSG_MATCH
	GDAX_MSG_CHANGE
	GDAX_MSG_ACTIVATE
	GDAX_MSG_HEARTBEAT
	GDAX_MSG_ERROR
)

var GdaxMsgTypeString = map[GdaxMsgType]string{
	GDAX_MSG_UNKNOWN:   "",
	GDAX_MSG_RECEIVED:  "received",
	GDAX_MSG_OPEN:      "open",
	GDAX_MSG_DONE:      "done",
	GDAX_MSG_MATCH:     "match",
	GDAX_MSG_CHANGE:    "change",
	GDAX_MSG_ACTIVATE:  "activate",
	GDAX_MSG_HEARTBEAT: "heartbeat",
	GDAX_MSG_ERROR:     "error",
}

func (mt *GdaxMsgType) String() string {
	return GdaxMsgTypeString[*mt]
}

func (mt *GdaxMsgType) CanNotify(level int64, side model.OrderSide, reason GdaxMsgReason) bool {
	switch level {
	case notify.LEVEL_NOTHING:
		return false
	case notify.LEVEL_ERRORS:
		return *mt == GDAX_MSG_ERROR
	case notify.LEVEL_VERBOSE:
		return true
	}
	return (*mt == GDAX_MSG_ERROR) ||
		(*mt == GDAX_MSG_ACTIVATE) ||
		(*mt == GDAX_MSG_OPEN && side == model.SELL) ||
		(*mt == GDAX_MSG_DONE && reason == GDAX_REASON_FILLED)
}

func NewGdaxMsgType(data string) GdaxMsgType {
	for mt := range GdaxMsgTypeString {
		if mt.String() == data {
			return mt
		}
	}
	return GDAX_MSG_UNKNOWN
}

type GdaxMsgReason int

const (
	GDAX_REASON_UNKNOWN GdaxMsgReason = iota
	GDAX_REASON_FILLED
	GDAX_REASON_CANCELED
)

var GdaxMsgReasonString = map[GdaxMsgReason]string{
	GDAX_REASON_UNKNOWN:  "",
	GDAX_REASON_FILLED:   "filled",
	GDAX_REASON_CANCELED: "canceled",
}

func (mr *GdaxMsgReason) String() string {
	return GdaxMsgReasonString[*mr]
}

func NewGdaxMsgReason(data string) GdaxMsgReason {
	for mr := range GdaxMsgReasonString {
		if mr.String() == data {
			return mr
		}
	}
	return GDAX_REASON_UNKNOWN
}

type GdaxMessage struct {
	*exchange.Message
	UserId string `json:"user_id"`
}

func (msg *GdaxMessage) Title() string {
	var out string
	out = "Coinbase Pro - " + strings.Title(msg.Type) + " " + strings.Title(msg.Side)
	mt := NewGdaxMsgType(msg.Type)
	if mt == GDAX_MSG_DONE {
		if msg.Reason != "" {
			out = out + " (Reason: " + strings.Title(msg.Reason) + ")"
		}
	}
	return out
}

type gdaxProduct struct {
	Id             string `json:"id"`
	BaseCurrency   string `json:"base_currency"`
	QuoteCurrency  string `json:"quote_currency"`
	BaseMinSize    string `json:"base_min_size"`
	BaseMaxSize    string `json:"base_max_size"`
	BaseIncrement  string `json:"base_increment"`
	QuoteIncrement string `json:"quote_increment"`
	LimitOnly      bool   `json:"limit_only"`
}

func (product *gdaxProduct) getMinOrderSize() (float64, error) {
	out, err := strconv.ParseFloat(product.BaseMinSize, 64)
	if err != nil {
		return 0, errors.Wrap(err, 1)
	} else {
		return out, nil
	}
}

type Gdax struct {
	*model.ExchangeInfo
	products []gdaxProduct
}

func (self *Gdax) error(err error, level int64, service model.Notify) {
	pc, file, line, _ := runtime.Caller(1)
	str := err.Error()
	log.Printf("[ERROR] %s %s", errors.FormatCaller(pc, file, line), strings.Replace(str, "\n\n", " ", -1))
	if service != nil {
		if notify.CanSend(level, notify.ERROR) {
			service.SendMessage(str, "Coinbase Pro - ERROR")
		}
	}
}

func (self *Gdax) getMinOrderSize(client *exchange.Client, market string) (float64, error) {
	cached := true
	for {
		products, err := self.GetProducts(client, cached)

		if err != nil {
			return 0, err
		}

		for _, product := range products {
			if product.Id == market {
				out, err := product.getMinOrderSize()
				if err != nil {
					return 0, err
				} else {
					return out, nil
				}
			}
		}

		if cached {
			cached = false
		} else {
			return 0, errors.Errorf("Market %s does not exist", market)
		}
	}
}

func (self *Gdax) GetInfo() *model.ExchangeInfo {
	return self.ExchangeInfo
}

func (self *Gdax) GetClient(permission model.Permission, sandbox bool) (interface{}, error) {
	if permission != model.PRIVATE {
		return exchange.NewClient("", "", "", sandbox), nil
	}

	var (
		err           error
		apiKey        string
		apiSecret     string
		apiPassphrase string
	)
	if apiKey, apiSecret, apiPassphrase, err = promptForApiKeysEx("Coinbase Pro"); err != nil {
		return nil, err
	}

	return exchange.NewClient(apiSecret, apiKey, apiPassphrase, sandbox), nil
}

func (self *Gdax) getProducts(client interface{}) ([]gdaxProduct, error) {
	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}
	var output []gdaxProduct
	_, err := gdaxClient.Request("GET", "/products", nil, &output)
	if err != nil {
		return nil, errors.Wrap(err, 1)
	}
	return output, nil
}

func (self *Gdax) GetProducts(client interface{}, cached bool) ([]gdaxProduct, error) {
	if self.products == nil || cached == false {
		var err error
		if self.products, err = self.getProducts(client); err != nil {
			return nil, err
		}
	}
	return self.products, nil
}

type gdaxMe struct {
	Id string `json:"id"`
}

func (self *Gdax) getMe(client interface{}) (*gdaxMe, error) {
	gdax, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}
	var out gdaxMe
	_, err := gdax.Request("GET", "/users/self", nil, &out)
	if err != nil {
		return nil, errors.Wrap(err, 1)
	}
	return &out, nil
}

func (self *Gdax) GetMarkets(cached, sandbox bool) ([]model.Market, error) {
	var out []model.Market

	products, err := self.GetProducts(exchange.NewClient("", "", "", sandbox), cached)

	if err != nil {
		return nil, err
	}

	for _, product := range products {
		out = append(out, model.Market{
			Name:  product.Id,
			Base:  product.BaseCurrency,
			Quote: product.QuoteCurrency,
		})
	}

	return out, nil
}

func (self *Gdax) FormatMarket(base, quote string) string {
	return fmt.Sprintf("%s-%s", base, quote)
}

func (self *Gdax) sell(
	hold model.Markets,
	apiSecret string,
	apiKey string,
	apiPassphrase string,
	gdaxUserId string,
	service model.Notify,
	twitter *notify.TwitterKeys,
	sandbox bool,
	debug bool,
	success model.OnSuccess,
) error {
	var err error

	URI := self.ExchangeInfo.WebSocket.URI
	if sandbox {
		URI = self.ExchangeInfo.WebSocket.Sandbox
	}

	var markets []model.Market
	if markets, err = self.GetMarkets(true, sandbox); err != nil {
		return err
	}

	var (
		conn *ws.Conn
		init func() (*ws.Conn, error)
	)

	init = func() (out *ws.Conn, err error) {
		if out, _, err = ws.DefaultDialer.Dial(URI, nil); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		var sub *gdaxSubscribePrivate
		if sub, err = self.newSubscribePrivate(apiSecret, apiKey, apiPassphrase, sandbox); err != nil {
			return nil, err
		}
		if err = out.WriteJSON(sub); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		log.Printf("[INFO] Listening to %s (user: %s)\n", URI, gdaxUserId)
		return out, nil
	}

	if conn, err = init(); err != nil {
		return err
	}

	if err = success(service); err != nil {
		return err
	}

	lastInterval := time.Now()
	const intervalMinutes = 1

	for {
		var data []byte
		_, data, err = conn.ReadMessage()
		if err != nil {
			// do we have a close error?
			ce, ok := err.(*ws.CloseError)
			if ok && ce.Code != ws.CloseNormalClosure {
				// restart the connection on abnormal closure
				log.Printf("[ERROR] %v", err)
				time.Sleep(5 * time.Second)
				conn, err = init()
				if err != nil {
					self.error(err, notify.Level(), service)
					return err
				}
			} else {
				// read: connection reset by peer?
				if strings.Contains(err.Error(), "connection reset by peer") {
					for true {
						log.Printf("[ERROR] %v", err)
						time.Sleep(5 * time.Second)
						conn, err = init()
						if err == nil {
							break
						} else {
							if !strings.Contains(err.Error(), "connection reset by peer") {
								self.error(err, notify.Level(), service)
								return err
							}
						}
					}
				} else {
					// exit the websocket loop
					self.error(err, notify.Level(), service)
					return err
				}
			}
		} else {
			msg := GdaxMessage{}
			if err = json.Unmarshal(data, &msg); err != nil {
				self.error(errors.Errorf("%s. Message: %s", err.Error(), string(data)), notify.Level(), service)
				return err
			}
			if NewGdaxMsgType(msg.Type) == GDAX_MSG_ERROR {
				self.error(errors.Errorf("%s. Reason: %s", msg.Message.Message, msg.Reason), notify.Level(), service)
				return err
			}
			if msg.UserId == gdaxUserId {
				log.Printf("[INFO] %s", string(data))
				mt := NewGdaxMsgType(msg.Type)
				// send a notification if type != ["received"|"match"]
				if mt != GDAX_MSG_RECEIVED && mt != GDAX_MSG_MATCH {
					if mt.CanNotify(notify.Level(), model.NewOrderSide(msg.Side), NewGdaxMsgReason(msg.Reason)) {
						if service != nil {
							if err = service.SendMessage(string(data), msg.Title()); err != nil {
								log.Printf("[ERROR] %v", err)
							}
						}
						if twitter != nil {
							if mt == GDAX_MSG_DONE && NewGdaxMsgReason(msg.Reason) == GDAX_REASON_FILLED {
								notify.Tweet(twitter, fmt.Sprintf("Done %s. %s priced at %f #CoinbasePro", strings.Title(msg.Side), model.TweetMarket(markets, msg.ProductId), msg.Price))
							}
						}
					}
				}
				// has a buy order been filled? then place a sell order
				if mt == GDAX_MSG_DONE {
					mr := NewGdaxMsgReason(msg.Reason)
					if mr == GDAX_REASON_FILLED {
						side := model.NewOrderSide(msg.Side)
						if side == model.BUY {
							client := exchange.NewClient(apiSecret, apiKey, apiPassphrase, sandbox)

							price := msg.Price
							if price == 0 {
								if price, err = self.GetTicker(client, msg.ProductId); err != nil {
									self.error(err, notify.Level(), service)
								}
							}

							var old exchange.Order
							if old, err = client.GetOrder(msg.OrderId); err != nil {
								self.error(errors.Wrap(err, 1), notify.Level(), service)
							}

							qty := old.Size
							if qty == 0 {
								if qty, err = self.getMinOrderSize(client, msg.ProductId); err != nil {
									self.error(err, notify.Level(), service)
								}
								qty = qty * 5
							}

							var (
								base  string
								quote string
							)
							if base, quote, err = model.ParseMarket(markets, msg.ProductId); err != nil {
								if markets, err = self.GetMarkets(false, sandbox); err == nil {
									base, quote, err = model.ParseMarket(markets, msg.ProductId)
								}
								if err != nil {
									self.error(err, notify.Level(), service)
								}
							}

							var prec int
							if prec, err = self.GetPricePrec(client, msg.ProductId); err != nil {
								self.error(err, notify.Level(), service)
							}

							// by default, we will sell at a 5% profit
							order := exchange.Order{
								Type:      model.OrderTypeString[model.LIMIT],
								Side:      model.OrderSideString[model.SELL],
								Size:      self.GetMaxSize(client, base, quote, hold.HasMarket(msg.ProductId), qty),
								Price:     pricing.Multiply(price, model.GetMult(), prec),
								ProductId: msg.ProductId,
							}

							// log the newly created SELL order
							var raw []byte
							if raw, err = json.Marshal(order); err == nil {
								log.Println("[INFO] " + string(raw))
								if service != nil {
									if notify.CanSend(notify.Level(), notify.INFO) {
										service.SendMessage(string(raw), "Coinbase Pro - New Sell")
									}
								}
							}

							if _, err = client.CreateOrder(&order); err != nil {
								self.error(errors.Wrap(err, 1), notify.Level(), service)
							}
						}
					}
				}
			}
		}

		if time.Since(lastInterval).Minutes() > intervalMinutes {
			var (
				cursor *exchange.Cursor
				orders []exchange.Order
			)

			strategy := model.GetStrategy()

			// follow up on the "aggressive" strategy
			if strategy == model.STRATEGY_STANDARD && flag.Exists("dca") {
				// we won't be re-buying *unless* your most recent (non-sold) sell is older than 14 days
				const rebuyAfterDays = 14
				var open []exchange.Order

				client := exchange.NewClient(apiSecret, apiKey, apiPassphrase, sandbox)

				cursor = client.ListOrders(exchange.ListOrdersParams{Status: "open"})
				for cursor.HasMore {
					if err = cursor.NextPage(&orders); err != nil {
						break
					} else {
						for _, order := range orders {
							open = append(open, order)
						}
					}
				}

				if err != nil {
					self.error(errors.Wrap(err, 1), notify.Level(), service)
				} else {
					var products []gdaxProduct
					if products, err = self.GetProducts(client, true); err != nil {
						self.error(err, notify.Level(), service)
					} else {
						for _, product := range products {
							if product.LimitOnly {
								// ignore this market because it is limit-only
							} else {
								youngest := time.Time{} // January 1, year 1, 00:00:00.000000000 UTC

								for _, order := range open {
									side := model.NewOrderSide(order.Side)
									if side == model.SELL {
										if order.ProductId == product.Id {
											createdAt := order.CreatedAt.Time()
											if youngest.IsZero() || youngest.Before(createdAt) {
												youngest = createdAt
											}
										}
									}
								}

								if !youngest.IsZero() && time.Since(youngest).Hours() > 24*rebuyAfterDays {
									// did we recently sell an "aggressive" order on this market? then prevent us from buying this pump.
									var closed model.Orders
									if closed, err = self.GetClosed(client, product.Id); err != nil {
										self.error(err, notify.Level(), service)
									} else {
										if time.Since(closed.Youngest(model.SELL, time.Now())).Hours() < 24*rebuyAfterDays {
											// continue
										} else {
											msg := fmt.Sprintf(
												"Re-buying %s because your latest activity on this market (at %s) is older than %d days.",
												product.Id, youngest.Format(time.RFC1123), rebuyAfterDays,
											)

											log.Println("[INFO] " + msg)
											if service != nil {
												if notify.CanSend(notify.Level(), notify.INFO) {
													service.SendMessage(msg, "Coinbase Pro - INFO")
												}
											}

											var qty float64
											if qty, err = product.getMinOrderSize(); err != nil {
												self.error(err, notify.Level(), service)
											} else {
												if hold.HasMarket(product.Id) {
													qty = qty * 5
												}

												order := exchange.Order{
													Type:      model.OrderTypeString[model.MARKET],
													Side:      model.OrderSideString[model.BUY],
													Size:      qty,
													ProductId: product.Id,
												}

												// log the newly created BUY order
												var raw []byte
												if raw, err = json.Marshal(order); err == nil {
													log.Println("[INFO] " + string(raw))
													if service != nil {
														if notify.CanSend(notify.Level(), notify.INFO) {
															service.SendMessage(string(raw), "Coinbase Pro - New Buy")
														}
													}
												}

												if _, err = client.CreateOrder(&order); err != nil {
													self.error(errors.Wrap(err, 1), notify.Level(), service)
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

			// follow up on the trailing stop loss strategy
			if strategy == model.STRATEGY_TRAILING || strategy == model.STRATEGY_TRAILING_STOP_LOSS {
				mult := model.GetMult()
				cache := make(map[string]float64)
				client := exchange.NewClient(apiSecret, apiKey, apiPassphrase, sandbox)

				// phase #1: enumerate over limit sell orders
				cursor = client.ListOrders(exchange.ListOrdersParams{Status: "open"})
				for cursor.HasMore {
					if err = cursor.NextPage(&orders); err != nil {
						self.error(errors.Wrap(err, 1), notify.Level(), service)
					} else {
						for _, order := range orders {
							// do not replace the limit orders that are merely used as a reference for the HODL strategy
							if !hold.HasMarket(order.ProductId) {
								// replace limit sell (not limit buy) orders
								side := model.NewOrderSide(order.Side)
								if side == model.SELL {
									ticker, ok := cache[order.ProductId]
									if !ok {
										if ticker, err = self.GetTicker(client, order.ProductId); err == nil {
											cache[order.ProductId] = ticker
										}
									}
									if ticker > 0 {
										var price float64
										price = pricing.NewMult(mult, 0.75) * (order.Price / mult)
										// is the ticker nearing the price? then cancel the limit sell order, and place a stop loss order below the ticker.
										if ticker > price {
											if err = client.CancelOrder(order.Id); err == nil {
												var prec int
												if prec, err = self.GetPricePrec(client, order.ProductId); err == nil {
													price = pricing.NewMult(mult, 0.5) * (ticker / mult)
													_, err = self.StopLoss(client, order.ProductId, order.Size, pricing.RoundToPrecision(price, prec), model.LIMIT, "")
													// is this market in limit-only mode?
													if err != nil {
														log.Printf("[ERROR] %v\n", err)
														_, err = client.CreateOrder(&order)
													}
												}
											}
										}
									}
									if err != nil {
										msg := err.Error()
										var data []byte
										if data, err = json.Marshal(order); err == nil {
											msg = msg + "\n\n" + string(data)
										}
										self.error(errors.New(msg), notify.Level(), service)
									}
								}
							}
						}
					}
				}

				// phase #2: enumerate over stop loss orders
				cursor = client.ListOrders(exchange.ListOrdersParams{Status: "active"})
				for cursor.HasMore {
					if err = cursor.NextPage(&orders); err != nil {
						self.error(errors.Wrap(err, 1), notify.Level(), service)
					} else {
						for _, order := range orders {
							// replace stop loss (not stop entry) orders
							if order.Stop == "loss" {
								ticker, ok := cache[order.ProductId]
								if !ok {
									if ticker, err = self.GetTicker(client, order.ProductId); err == nil {
										cache[order.ProductId] = ticker
									}
								}
								if ticker > 0 {
									var prec int
									if prec, err = self.GetPricePrec(client, order.ProductId); err == nil {
										var price float64
										price = pricing.NewMult(mult, 0.5) * (ticker / mult)
										// is the distance bigger than 5%? then cancel the stop loss, and place a new one.
										if order.StopPrice < pricing.RoundToPrecision(price, prec) {
											if err = client.CancelOrder(order.Id); err == nil {
												_, err = self.StopLoss(client, order.ProductId, order.Size, pricing.RoundToPrecision(price, prec), model.LIMIT, "")
											}
										}
									}
								}
								if err != nil {
									msg := err.Error()
									var data []byte
									if data, err = json.Marshal(order); err == nil {
										msg = msg + "\n\n" + string(data)
									}
									self.error(errors.New(msg), notify.Level(), service)
								}
							}
						}
					}
				}
			}
			lastInterval = time.Now()
		}
	}
}

func (self *Gdax) Sell(
	start time.Time,
	hold model.Markets,
	sandbox, tweet, debug bool,
	success model.OnSuccess,
) error {
	var err error

	strategy := model.GetStrategy()
	if strategy == model.STRATEGY_STANDARD || strategy == model.STRATEGY_TRAILING {
		// we are OK
	} else {
		return errors.New("Strategy not implemented")
	}

	var (
		apiSecret     string
		apiKey        string
		apiPassphrase string
		apiUserId     string
	)
	if apiKey, apiSecret, apiPassphrase, err = promptForApiKeysEx("Coinbase Pro"); err != nil {
		return err
	}

	apiUserId = flag.Get("api-user-id").String()
	if apiUserId == "" {
		// get the GDAX user ID
		client := exchange.NewClient(apiSecret, apiKey, apiPassphrase, sandbox)
		var me *gdaxMe
		if me, err = self.getMe(client); err != nil {
			return err
		}
		apiUserId = me.Id
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

	return self.sell(hold, apiSecret, apiKey, apiPassphrase, apiUserId, service, twitter, sandbox, debug, success)
}

func (self *Gdax) Order(
	client interface{},
	side model.OrderSide,
	market string,
	size float64,
	price float64,
	kind model.OrderType,
	meta string,
) (oid []byte, raw []byte, err error) {
	gdax, ok := client.(*exchange.Client)
	if !ok {
		return nil, nil, errors.New("invalid argument: client")
	}

	order := exchange.Order{
		Type:      model.OrderTypeString[model.LIMIT],
		Side:      model.OrderSideString[side],
		Size:      size,
		Price:     price,
		ProductId: market,
	}

	var saved exchange.Order
	if saved, err = gdax.CreateOrder(&order); err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}

	var out []byte
	if out, err = json.Marshal(saved); err != nil {
		return nil, nil, errors.Wrap(err, 1)
	}

	return []byte(saved.Id), out, nil
}

func (self *Gdax) StopLoss(client interface{}, market string, size float64, price float64, kind model.OrderType, meta string) ([]byte, error) {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	order := exchange.Order{
		Type:      model.OrderTypeString[kind],
		Side:      model.OrderSideString[model.SELL],
		Size:      size,
		ProductId: market,
		Stop:      "loss",
		StopPrice: price,
	}

	if kind == model.LIMIT {
		var prec int
		if prec, err = self.GetPricePrec(client, order.ProductId); err != nil {
			return nil, err
		}
		limit := price
		for true {
			limit = limit * 0.99
			if pricing.RoundToPrecision(limit, prec) < price {
				break
			}
		}
		order.Price = pricing.RoundToPrecision(limit, prec)
	}

	var saved exchange.Order
	if saved, err = gdaxClient.CreateOrder(&order); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	var out []byte
	if out, err = json.Marshal(saved); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	return out, nil
}

func (self *Gdax) OCO(client interface{}, market string, size float64, price, stop float64, meta1, meta2 string) ([]byte, error) {
	return nil, errors.New("Not implemented")
}

func (self *Gdax) GetClosed(client interface{}, market string) (model.Orders, error) {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	params := exchange.ListFillsParams{
		ProductId: market,
	}
	cursor := gdaxClient.ListFills(params)

	var out model.Orders
	var fills []exchange.Fill
	for cursor.HasMore {
		if err = cursor.NextPage(&fills); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		for _, fill := range fills {
			out = append(out, model.Order{
				Side:      model.NewOrderSide(fill.Side),
				Market:    fill.ProductId,
				Size:      fill.Size,
				Price:     fill.Price,
				CreatedAt: fill.CreatedAt.Time(),
			})
		}
	}

	return out, nil
}

func (self *Gdax) GetOpened(client interface{}, market string) (model.Orders, error) {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	params := exchange.ListOrdersParams{
		Status: "open",
	}
	cursor := gdaxClient.ListOrders(params)

	var out model.Orders
	var orders []exchange.Order
	for cursor.HasMore {
		if err = cursor.NextPage(&orders); err != nil {
			return nil, errors.Wrap(err, 1)
		}
		for _, order := range orders {
			if order.ProductId == market {
				out = append(out, model.Order{
					Side:      model.NewOrderSide(order.Side),
					Market:    order.ProductId,
					Size:      order.Size,
					Price:     order.Price,
					CreatedAt: order.CreatedAt.Time(),
				})
			}
		}
	}

	return out, nil
}

func (self *Gdax) GetBook(client interface{}, market string, side model.BookSide) (interface{}, error) {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	var book exchange.Book
	if book, err = gdaxClient.GetBook(market, 3); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	var out []exchange.BookEntry
	if side == model.BOOK_SIDE_ASKS {
		out = book.Asks
	} else {
		out = book.Bids
	}

	return out, nil
}

func (self *Gdax) Aggregate(client, book interface{}, market string, agg float64) (model.Book, error) {
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
		price := pricing.RoundToPrecision(pricing.RoundToNearest(e.Price, agg), prec)
		entry := out.EntryByPrice(price)
		if entry != nil {
			entry.Size = entry.Size + e.Size
		} else {
			entry = &model.BookEntry{
				Buy: &model.Buy{
					Market: market,
					Price:  price,
				},
				Size: e.Size,
			}
			out = append(out, *entry)
		}

	}

	return out, nil
}

func (self *Gdax) GetTicker(client interface{}, market string) (float64, error) {
	var err error

	gdax, ok := client.(*exchange.Client)
	if !ok {
		return 0, errors.New("invalid argument: client")
	}

	var ticker exchange.Ticker
	if ticker, err = gdax.GetTicker(market); err != nil {
		return 0, err
	}

	return ticker.Price, nil
}

func (self *Gdax) Get24h(client interface{}, market string) (*model.Stats, error) {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return nil, errors.New("invalid argument: client")
	}

	var gdaxStats exchange.Stats
	if gdaxStats, err = gdaxClient.GetStats(market); err != nil {
		return nil, errors.Wrap(err, 1)
	}

	return &model.Stats{
		Market:    market,
		High:      gdaxStats.High,
		Low:       gdaxStats.Low,
		BtcVolume: 0,
	}, nil
}

func (self *Gdax) GetPricePrec(client interface{}, market string) (int, error) {
	products, err := self.GetProducts(client, true)
	if err != nil {
		return 0, err
	}
	for _, p := range products {
		if p.Id == market {
			return getPrecFromStr(p.QuoteIncrement, 0), nil
		}
	}
	return 0, errors.Errorf("market %s not found", market)
}

func (self *Gdax) GetSizePrec(client interface{}, market string) (int, error) {
	products, err := self.GetProducts(client, true)
	if err != nil {
		return 8, err
	}
	for _, p := range products {
		if p.Id == market {
			return getPrecFromStr(p.BaseIncrement, 8), nil
		}
	}
	return 8, errors.Errorf("market %s not found", market)
}

func (self *Gdax) GetMaxSize(client interface{}, base, quote string, hold bool, def float64) float64 {
	market := self.FormatMarket(base, quote)

	fn := func() int {
		prec, err := self.GetSizePrec(client, market)
		if err != nil {
			return 8
		} else {
			return prec
		}
	}

	out := model.GetSizeMax(hold, def, fn)

	if hold {
		gdaxClient, ok := client.(*exchange.Client)
		if ok {
			min, err := self.getMinOrderSize(gdaxClient, market)
			if err == nil {
				if min > out {
					out = min
				}
			}
		}
	}

	return out
}

func (self *Gdax) Cancel(client interface{}, market string, side model.OrderSide) error {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return errors.New("invalid argument: client")
	}

	params := exchange.ListOrdersParams{
		Status: "open",
	}
	cursor := gdaxClient.ListOrders(params)

	var orders []exchange.Order
	for cursor.HasMore {
		if err = cursor.NextPage(&orders); err != nil {
			return errors.Wrap(err, 1)
		}
		for _, order := range orders {
			if order.ProductId == market {
				if ((side == model.BUY) && (order.Side == model.OrderSideString[model.BUY])) || ((side == model.SELL) && (order.Side == model.OrderSideString[model.SELL])) {
					if err = gdaxClient.CancelOrder(order.Id); err != nil {
						return errors.Wrap(err, 1)
					}
				}
			}
		}
	}

	return nil
}

func (self *Gdax) Buy(client interface{}, cancel bool, market string, calls model.Calls, size, deviation float64, kind model.OrderType) error {
	var err error

	gdaxClient, ok := client.(*exchange.Client)
	if !ok {
		return errors.New("invalid argument: client")
	}

	// step #1: delete the buy order(s) that are open in your book
	if cancel {
		params := exchange.ListOrdersParams{
			Status: "open",
		}
		cursor := gdaxClient.ListOrders(params)
		var orders []exchange.Order
		for cursor.HasMore {
			if err = cursor.NextPage(&orders); err != nil {
				return errors.Wrap(err, 1)
			}
			for _, order := range orders {
				if order.ProductId == market {
					if order.Side == model.OrderSideString[model.BUY] {
						// do not cancel orders that we're about to re-place
						index := calls.IndexByPrice(order.Price)
						if index > -1 && order.Size == calls[index].Quantity {
							calls[index].Skip = true
						} else {
							if err = gdaxClient.CancelOrder(order.Id); err != nil {
								return errors.Wrap(err, 1)
							}
						}
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
			qty := call.Quantity
			if qty == 0 {
				qty = size
			}
			order := exchange.Order{
				Type:      model.OrderTypeString[model.LIMIT],
				Side:      model.OrderSideString[model.BUY],
				Size:      qty,
				Price:     limit,
				ProductId: market,
			}
			if _, err = gdaxClient.CreateOrder(&order); err != nil {
				var raw []byte
				if raw, _ = json.Marshal(order); raw == nil {
					return errors.Wrap(err, 1)
				} else {
					return errors.Wrap(errors.Append(err, "\t", string(raw)), 1)
				}
			}
		}
	}

	return nil
}

func (self *Gdax) IsLeveragedToken(name string) bool {
	return false
}

func NewGdax() model.Exchange {
	return &Gdax{
		ExchangeInfo: &model.ExchangeInfo{
			Code: "GDAX",
			Name: "Coinbase Pro",
			URL:  "https://pro.coinbase.com",
			REST: model.Endpoint{
				URI:     "https://api.pro.coinbase.com",
				Sandbox: "https://api-public.sandbox.pro.coinbase.com",
			},
			WebSocket: model.Endpoint{
				URI:     "wss://ws-feed.pro.coinbase.com",
				Sandbox: "wss://ws-feed-public.sandbox.pro.coinbase.com",
			},
			Country: "USA",
		},
	}
}

type gdaxSubscribePublic struct {
	Type       string   `json:"type"`
	ProductIDs []string `json:"product_ids"`
	Channels   []string `json:"channels"`
}

func (self *Gdax) newSubscribePublic(sandbox bool) (*gdaxSubscribePublic, error) {
	out := gdaxSubscribePublic{
		Type:       "subscribe",
		ProductIDs: []string{},
		Channels:   []string{"user", "heartbeat"},
	}
	products, err := self.GetProducts(exchange.NewClient("", "", "", sandbox), false)
	if err != nil {
		return nil, err
	}
	for _, product := range products {
		out.ProductIDs = append(out.ProductIDs, product.Id)
	}
	return &out, nil
}

type gdaxSubscribePrivate struct {
	*gdaxSubscribePublic
	Signature  string `json:"signature"`
	Key        string `json:"key"`
	Passphrase string `json:"passphrase"`
	Timestamp  string `json:"timestamp"`
}

func (self *Gdax) newSubscribePrivate(secret, key, passphrase string, sandbox bool) (*gdaxSubscribePrivate, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	message := fmt.Sprintf("%s%s%s%s", timestamp, "GET", "/users/self/verify", "")

	signature, err := gdaxGenerateSignature(message, secret)
	if err != nil {
		return nil, err
	}

	public, err := self.newSubscribePublic(sandbox)
	if err != nil {
		return nil, err
	}

	return &gdaxSubscribePrivate{
		gdaxSubscribePublic: public,
		Signature:           signature,
		Key:                 key,
		Passphrase:          passphrase,
		Timestamp:           timestamp,
	}, nil
}

func gdaxGenerateSignature(message, secret string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", errors.Wrap(err, 1)
	}

	signature := hmac.New(sha256.New, key)
	_, err = signature.Write([]byte(message))
	if err != nil {
		return "", errors.Wrap(err, 1)
	}

	return base64.StdEncoding.EncodeToString(signature.Sum(nil)), nil
}
