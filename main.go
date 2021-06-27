package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/go-errors/errors"
	"github.com/gorilla/mux"
	"github.com/mitchellh/cli"
	"github.com/svanas/nefertiti/command"
	ctcommand "github.com/svanas/nefertiti/command"
	"github.com/svanas/nefertiti/flag"
)

const (
	APP_NAME    = "nefertiti"
)

var (
	GitSummary string = "0.0.154"
	APP_VERSION = strings.TrimPrefix(GitSummary, "v")
	console *cli.CLI
	port    int64 = 38700
)

func main() {
	var (
		err  error
		cnt  uintptr
		file string
		line int
	)

	var cb ctcommand.CommandCallBack
	cb = func(pc uintptr, fn string, ln int, e error) {
		err = e
		cnt = pc
		file = fn
		line = ln
	}

	cm := ctcommand.CommandMeta{
		Port:       &port,
		AppName:    APP_NAME,
		AppVersion: APP_VERSION,
		CallBack:   &cb,
	}

	console = cli.NewCLI(APP_NAME, APP_VERSION)
	console.Args = os.Args[1:]
	console.Commands = map[string]cli.CommandFactory{
		"exchanges": func() (cli.Command, error) {
			return &ctcommand.ExchangesCommand{&cm}, nil
		},
		"markets": func() (cli.Command, error) {
			return &ctcommand.MarketsCommand{&cm}, nil
		},
		"sell": func() (cli.Command, error) {
			return &ctcommand.SellCommand{&cm}, nil
		},
		"order": func() (cli.Command, error) {
			return &ctcommand.OrderCommand{&cm}, nil
		},
		"book": func() (cli.Command, error) {
			return &ctcommand.BookCommand{&cm}, nil
		},
		"buy": func() (cli.Command, error) {
			return &ctcommand.BuyCommand{&cm}, nil
		},
		"about": func() (cli.Command, error) {
			return &ctcommand.AboutCommand{&cm}, nil
		},
		"update": func() (cli.Command, error) {
			return &ctcommand.UpdateCommand{&cm}, nil
		},
		"agg": func() (cli.Command, error) {
			return &ctcommand.AggCommand{&cm}, nil
		},
		"cancel": func() (cli.Command, error) {
			return &ctcommand.CancelCommand{&cm}, nil
		},
		"base": func() (cli.Command, error) {
			return &ctcommand.BaseCommand{&cm}, nil
		},
		"quote": func() (cli.Command, error) {
			return &ctcommand.QuoteCommand{&cm}, nil
		},
		"notify": func() (cli.Command, error) {
			return &ctcommand.NotifyCommand{&cm}, nil
		},
		"stoploss": func() (cli.Command, error) {
			return &ctcommand.StopLossCommand{&cm}, nil
		},
		"listen": func() (cli.Command, error) {
			return &ctcommand.ListenCommand{&cm}, nil
		},
	}

	if flag.Listen() {
		go func() {
			var (
				err    error
				router *mux.Router
			)

			router = mux.NewRouter()
			router.HandleFunc("/ping", ping).Host("127.0.0.1").Methods(http.MethodGet)
			router.HandleFunc("/post", post).Host("127.0.0.1").Methods(http.MethodPost)
			router.HandleFunc("/", delete).Host("127.0.0.1").Methods(http.MethodDelete)

			flg := flag.Get("port")
			if flg.Exists {
				if port, err = flg.Int64(); err != nil {
					log.Printf("[ERROR] %v", err)
					os.Exit(1)
				}
				if err = http.ListenAndServe(fmt.Sprintf(":%d", port), router); err != nil {
					log.Printf("[ERROR] %v", err)
					os.Exit(1)
				}
			} else {
				for true {
					err = http.ListenAndServe(fmt.Sprintf(":%d", port), router)
					if err == nil {
						break
					} else {
						if strings.Contains(err.Error(), "address already in use") {
							port++
						} else {
							log.Printf("[ERROR] %v", err)
							os.Exit(1)
						}
					}
				}
			}
		}()
	}

	code, _ := console.Run()

	if err != nil {
		prefix := errors.FormatCaller(cnt, file, line)
		_, ok := err.(*errors.Error)
		if ok && flag.Debug() {
			log.Printf("[ERROR] %s", err.(*errors.Error).ErrorStack(prefix, ""))
		} else {
			log.Printf("[ERROR] %s", fmt.Sprintf("%s %v", prefix, err))
		}
		if code != 0 {
			os.Exit(code)
		} else {
			os.Exit(1)
		}
	}

	os.Exit(code)
}

// GET 127.0.0.1:[port]/ping

func getPong() *command.Pong {
	out := command.Pong{
		Port:    port,
		Command: console.Subcommand(),
	}
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-") {
			for true {
				arg = arg[1:]
				if !strings.HasPrefix(arg, "-") {
					break
				}
			}
			if strings.HasPrefix(arg, "hub=") || strings.HasPrefix(arg, "port=") ||
				strings.HasPrefix(arg, "api-key") || strings.HasPrefix(arg, "api-secret") || strings.HasPrefix(arg, "api-passphrase") {
				// nothing
			} else {
				out.Args = append(out.Args, arg)
			}
		}
	}
	return &out
}

func ping(resp http.ResponseWriter, req *http.Request) {
	json.NewEncoder(resp).Encode(getPong())
}

// POST 127.0.0.1:[port]/post

func post(resp http.ResponseWriter, req *http.Request) {
	req.ParseForm()
	for key, value := range req.Form {
		if key != "" && key != "port" && key != "command" {
			if value == nil {
				flag.Set(key, "")
			} else {
				flag.Set(key, value[0])
			}
		}
	}
	json.NewEncoder(resp).Encode(getPong())
}

// DELETE 127.0.0.1:[port]

func delete(resp http.ResponseWriter, req *http.Request) {
	resp.Write([]byte(""))
	defer os.Exit(0)
}
