package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	. "github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
	"golang.org/x/text/language"
	"net/http"
	"strings"
	"time"
)

func init() {
	RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() ChannelHandler {
	return &handler{handlers.NewBaseHandler(ChannelType("WS"), "WebSocket")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "register", h.registerUser)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveMessage)
	return nil
}

// receiveMessage is our HTTP handler function for incoming messages
func (h *handler) registerUser(ctx context.Context, channel Channel, w http.ResponseWriter, r *http.Request) ([]Event, error) {
	payload := &userPayload{}
	err := handlers.DecodeAndValidateJSON(payload, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// no URN? ignore this
	if payload.URN == "" {
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, "Ignoring request, no identifier")
	}

	// the list of data we will return in our response
	data := make([]interface{}, 0, 2)

	// create our URN
	urn, errURN := urns.NewURNFromParts(channel.Schemes()[0], payload.URN, "", "")
	if errURN != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, errURN)
	}

	contact, errGetContact := h.Backend().GetContact(ctx, channel, urn, "", "")
	if errGetContact != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, errGetContact)
	}

	// Getting the language in ISO3
	tag := language.MustParse(payload.Language)
	languageBase, _ := tag.Base()

	_, errLang := h.Backend().AddLanguageToContact(ctx, channel, languageBase.ISO3(), contact)
	if errLang != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, errLang)
	}

	// build our response
	data = append(data, NewEventRegisteredContactData(contact.UUID()))

	return nil, WriteDataResponse(ctx, w, http.StatusOK, "Events Handled", data)
}

// receiveMessage is our HTTP handler function for incoming messages
func (h *handler) receiveMessage(ctx context.Context, channel Channel, w http.ResponseWriter, r *http.Request) ([]Event, error) {
	payload := &moPayload{}
	err := handlers.DecodeAndValidateJSON(payload, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// no message? ignore this
	if payload.InstanceID == "" {
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, "Ignoring request, no message")
	}

	// the list of events we deal with
	events := make([]courier.Event, 0, 2)

	// the list of data we will return in our response
	data := make([]interface{}, 0, 2)

	for i := range payload.Messages {
		message := payload.Messages[i]

		if message.FromMe == false {
			// create our date from the timestamp
			date := time.Unix(message.Time, 0).UTC()

			// create our URN
			author := message.Author
			contactPhoneNumber := strings.Replace(author, "@c.us", "", 1)
			urn, errURN := urns.NewWhatsAppURN(contactPhoneNumber)
			if errURN != nil {
				return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, errURN)
			}

			// build our name from first and last
			name := handlers.NameFromFirstLastUsername(message.SenderName, "", "")

			// our text is either "text" or "caption" (or empty)
			text := message.Body
			isAttachment := false
			if message.Type == "image" {
				text = message.Caption
				isAttachment = true
			}

			// build our msg
			ev := h.Backend().NewIncomingMsg(channel, urn, text).WithExternalID(message.ID).WithReceivedOn(date).WithContactName(name)
			event := h.Backend().CheckExternalIDSeen(ev)

			if isAttachment {
				event.WithAttachment(message.Body)
			}

			errMsg := h.Backend().WriteMsg(ctx, event)
			if errMsg != nil {
				return nil, errMsg
			}

			h.Backend().WriteExternalIDSeen(event)

			events = append(events, event)
			data = append(data, courier.NewMsgReceiveData(event))
		}
	}

	for i := range payload.Ack {
		ack := payload.Ack[i]
		status := courier.MsgQueued

		if ack.Status == "sent" {
			status = courier.MsgSent
		} else if ack.Status == "delivered" {
			status = courier.MsgDelivered
		}

		event := h.Backend().NewMsgStatusForExternalID(channel, ack.ID, status)
		err := h.Backend().WriteMsgStatus(ctx, event)

		// we don't know about this message, just tell them we ignored it
		if err == courier.ErrMsgNotFound {
			data = append(data, courier.NewInfoData("message not found, ignored"))
			continue
		}

		if err != nil {
			return nil, err
		}

		events = append(events, event)
		data = append(data, courier.NewStatusData(event))

	}

	return events, courier.WriteDataResponse(ctx, w, http.StatusOK, "Events Handled", data)

}

func (h *handler) sendMsgPart(msg Msg, apiURL string, payload *dataPayload) (string, *ChannelLog, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		log := NewChannelLog("unable to build JSON body", msg.Channel(), msg.ID(), "", "", NilStatusCode, "", "", time.Duration(0), err)
		return "", log, err
	}

	req, _ := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rr, err := utils.MakeHTTPRequest(req)

	// build our channel log
	log := NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)

	return "", log, nil
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg Msg) (MsgStatus, error) {
	address := msg.Channel().Address()

	data := &dataPayload{
		ID:          msg.ID().String(),
		Text:        msg.Text(),
		To:          msg.URN().Path(),
		ToNoPlus:    strings.Replace(msg.URN().Path(), "+", "", 1),
		From:        address,
		FromNoPlus:  strings.Replace(address, "+", "", 1),
		Channel:     strings.Replace(address, "+", "", 1),
		Metadata:    nil,
		Attachments: nil,
	}

	if len(msg.QuickReplies()) > 0 {
		quickReplies := make(map[string][]string, 0)
		quickReplies["quick_replies"] = msg.QuickReplies()
		data.Metadata = quickReplies
	}

	if len(msg.Attachments()) > 0 {
		data.Attachments = msg.Attachments()
	}

	// the status that will be written for this message
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), MsgErrored)

	// whether we encountered any errors sending any parts
	hasError := true

	// if we have text, send that if we aren't sending it as a caption
	if msg.Text() != "" {
		externalID, log, err := h.sendMsgPart(msg, address, data)
		status.SetExternalID(externalID)
		hasError = err != nil
		status.AddLog(log)
	}

	if !hasError {
		status.SetStatus(MsgWired)
	}

	return status, nil
}

type userPayload struct {
	URN      string `json:"urn"`
	Language string `json:"language"`
}

type msgPayload struct {
	Text     string `json:"text"`
	UserURN  string `json:"userUrn"`
	UserUUID string `json:"userUuid"`
}

type dataPayload struct {
	ID          string
	Text        string
	To          string
	ToNoPlus    string
	From        string
	FromNoPlus  string
	Channel     string
	Metadata    map[string][]string
	Attachments []string
}
