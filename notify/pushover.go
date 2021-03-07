package notify

import (
	"errors"
	"strings"

	"github.com/gregdel/pushover"
	"github.com/svanas/nefertiti/flag"
	"github.com/svanas/nefertiti/model"
	"github.com/svanas/nefertiti/passphrase"
)

type Pushover struct {
	appKey  string
	userKey string
}

func (this *Pushover) promptForKeys(interactive bool) (ok bool, err error) {
	this.appKey = flag.Get("pushover-app-key").String()

	if this.appKey == "none" {
		return false, nil
	}

	if this.appKey == "" && interactive {
		var data []byte
		if data, err = passphrase.Read("Pushover application key"); err != nil {
			return false, err
		}
		this.appKey = string(data)
	}

	if this.appKey == "none" {
		return false, nil
	}

	if this.appKey != "" {
		this.userKey = flag.Get("pushover-user-key").String()
		if this.userKey == "" && interactive {
			var data []byte
			if data, err = passphrase.Read("Pushover user key"); err != nil {
				return false, err
			}
			this.userKey = string(data)
		}
	}

	return this.appKey != "" && this.userKey != "", nil
}

func (this *Pushover) PromptForKeys(interactive, verify bool) (ok bool, err error) {
	ok, err = this.promptForKeys(interactive)
	if ok && verify {
		// verify the pushover user key
		if err = PushoverVerifyRecipient(this.appKey, this.userKey); err != nil {
			ok = false
		}
	}
	return
}

func PushoverVerifyRecipient(appToken, userToken string) error {
	if appToken == "" {
		return errors.New("missing argument: Pushover application key")
	}
	app := pushover.New(appToken)
	rec := pushover.NewRecipient(userToken)
	_, err := app.GetRecipientDetails(rec)
	return err
}

func (this *Pushover) SendMessage(message, title string) error {
	if this.appKey == "" {
		return errors.New("missing argument: Pushover application key")
	}
	if this.userKey == "" {
		return errors.New("missing argument: Pushover recipient")
	}
	app := pushover.New(this.appKey)
	rec := pushover.NewRecipient(this.userKey)
	msg := pushover.NewMessageWithTitle(message, title)
	if result := strings.SplitN(message, ":::", 2); len(result) > 1 {
		msg.Sound = result[0]
		msg.Message = result[1]
	}
	_, err := app.SendMessage(msg, rec)
	return err
}

func NewPushover() model.Notify {
	return &Pushover{}
}
