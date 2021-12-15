/*
 * Copyright 2014 Canonical Ltd.
 *
 * Authors:
 * Sergio Schvezov: sergio.schvezov@cannical.com
 *
 * This file is part of telepathy.
 *
 * mms is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; version 3.
 *
 * mms is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package telepathy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/ubports/nuntium/mms"
	"github.com/ubports/nuntium/storage"
	"github.com/ubports/nuntium/telepathy/history"
	"launchpad.net/go-dbus/v1"
)

//Payload is used to build the dbus messages; this is a workaround as v1 of go-dbus
//tries to encode and decode private fields.
type Payload struct {
	Path       dbus.ObjectPath
	Properties map[string]dbus.Variant
}

type MMSService struct {
	payload              Payload
	Properties           map[string]dbus.Variant
	conn                 *dbus.Connection
	msgChan              chan *dbus.Message
	messageHandlers      map[dbus.ObjectPath]*MessageInterface
	msgDeleteChan        chan dbus.ObjectPath
	msgRedownloadChan    chan dbus.ObjectPath
	identity             string
	outMessage           chan *OutgoingMessage
	mNotificationIndChan chan<- *mms.MNotificationInd
}

type Attachment struct {
	Id        string
	MediaType string
	FilePath  string
	Offset    uint64
	Length    uint64
}

type OutAttachment struct {
	Id          string
	ContentType string
	FilePath    string
}

type OutgoingMessage struct {
	Recipients  []string
	Attachments []OutAttachment
	Reply       *dbus.Message
}

func NewMMSService(conn *dbus.Connection, modemObjPath dbus.ObjectPath, identity string, outgoingChannel chan *OutgoingMessage, useDeliveryReports bool, mNotificationIndChan chan<- *mms.MNotificationInd) *MMSService {
	properties := make(map[string]dbus.Variant)
	properties[identityProperty] = dbus.Variant{identity}
	serviceProperties := make(map[string]dbus.Variant)
	serviceProperties[useDeliveryReportsProperty] = dbus.Variant{useDeliveryReports}
	serviceProperties[modemObjectPathProperty] = dbus.Variant{modemObjPath}
	payload := Payload{
		Path:       dbus.ObjectPath(MMS_DBUS_PATH + "/" + identity),
		Properties: properties,
	}
	service := MMSService{
		payload:              payload,
		Properties:           serviceProperties,
		conn:                 conn,
		msgChan:              make(chan *dbus.Message),
		msgDeleteChan:        make(chan dbus.ObjectPath),
		msgRedownloadChan:    make(chan dbus.ObjectPath),
		messageHandlers:      make(map[dbus.ObjectPath]*MessageInterface),
		outMessage:           outgoingChannel,
		identity:             identity,
		mNotificationIndChan: mNotificationIndChan,
	}
	go service.watchDBusMethodCalls()
	go service.watchMessageDeleteCalls()
	go service.watchMessageRedownloadCalls()
	conn.RegisterObjectPath(payload.Path, service.msgChan)
	return &service
}

func (*MMSService) getMMSState(objectPath dbus.ObjectPath) (storage.MMSState, error) {
	uuid, err := getUUIDFromObjectPath(objectPath)
	if err != nil {
		return storage.MMSState{}, err
	}

	return storage.GetMMSState(uuid)
}

func (service *MMSService) watchMessageDeleteCalls() {
	for msgObjectPath := range service.msgDeleteChan {
		if mmsState, err := service.getMMSState(msgObjectPath); err == nil {
			if mmsState.State != storage.RESPONDED && mmsState.MNotificationInd != nil && !mmsState.MNotificationInd.Expired() {
				log.Printf("Message %s is not responded and not expired, not deleting.", string(msgObjectPath))
				continue
			}
		}

		if err := service.MessageRemoved(msgObjectPath); err != nil {
			log.Print("Failed to delete ", msgObjectPath, ": ", err)
		}
	}
}

func (service *MMSService) watchMessageRedownloadCalls() {
	for msgObjectPath := range service.msgRedownloadChan {
		mmsState, err := service.getMMSState(msgObjectPath)
		if err != nil {
			log.Printf("Redownload of %s error: retrieving message state error: %v", string(msgObjectPath), err)
			continue
		}
		if mmsState.State != storage.NOTIFICATION {
			log.Printf("Redownload of %s error: message was already downloaded", string(msgObjectPath))
			continue
		}
		if mmsState.MNotificationInd == nil {
			log.Printf("Redownload of %s error: no mNotificationInd found", string(msgObjectPath))
			continue
		}

		// Stop previous message handling, remove and notify.
		if err := service.MessageRemoved(msgObjectPath); err != nil {
			log.Printf("Redownload of %s warning: removing message error: %v", string(msgObjectPath), err)
		}

		// Start new mNotificationInd handling as if pushed from MMS service, but with info about redownload.
		newMNotificationInd := mmsState.MNotificationInd
		newMNotificationInd.RedownloadOfUUID = mmsState.MNotificationInd.UUID
		newMNotificationInd.UUID = mms.GenUUID()
		storage.Create(mmsState.ModemId, newMNotificationInd)
		service.mNotificationIndChan <- newMNotificationInd
	}
}

func (service *MMSService) watchDBusMethodCalls() {
	for msg := range service.msgChan {
		var reply *dbus.Message
		if msg.Interface != MMS_SERVICE_DBUS_IFACE {
			log.Println("Received unknown interface call on", msg.Interface, msg.Member)
			reply = dbus.NewErrorMessage(
				msg,
				"org.freedesktop.DBus.Error.UnknownInterface",
				fmt.Sprintf("No such interface '%s' at object path '%s'", msg.Interface, msg.Path),
			)
			if err := service.conn.Send(reply); err != nil {
				log.Println("Could not send reply:", err)
			}
			continue
		}
		switch msg.Member {
		case "GetMessages":
			reply = dbus.NewMethodReturnMessage(msg)
			//TODO implement store and forward
			var payload []Payload
			if err := reply.AppendArgs(payload); err != nil {
				log.Print("Cannot parse payload data from services")
				reply = dbus.NewErrorMessage(msg, "Error.InvalidArguments", "Cannot parse services")
			}
			if err := service.conn.Send(reply); err != nil {
				log.Println("Could not send reply:", err)
			}
		case "GetProperties":
			reply = dbus.NewMethodReturnMessage(msg)
			if pc, err := service.GetPreferredContext(); err == nil {
				service.Properties[preferredContextProperty] = dbus.Variant{pc}
			} else {
				// Using "/" as an invalid 'path' even though it could be considered 'incorrect'
				service.Properties[preferredContextProperty] = dbus.Variant{dbus.ObjectPath("/")}
			}
			if err := reply.AppendArgs(service.Properties); err != nil {
				log.Print("Cannot parse payload data from services")
				reply = dbus.NewErrorMessage(msg, "Error.InvalidArguments", "Cannot parse services")
			}
			if err := service.conn.Send(reply); err != nil {
				log.Println("Could not send reply:", err)
			}
		case "SetProperty":
			if err := service.setProperty(msg); err != nil {
				log.Println("Property set failed:", err)
				reply = dbus.NewErrorMessage(msg, "Error.InvalidArguments", err.Error())
			} else {
				reply = dbus.NewMethodReturnMessage(msg)
			}
			if err := service.conn.Send(reply); err != nil {
				log.Println("Could not send reply:", err)
			}
		case "SendMessage":
			var outMessage OutgoingMessage
			outMessage.Reply = dbus.NewMethodReturnMessage(msg)
			if err := msg.Args(&outMessage.Recipients, &outMessage.Attachments); err != nil {
				log.Print("Cannot parse payload data from services")
				reply = dbus.NewErrorMessage(msg, "Error.InvalidArguments", "Cannot parse New Message")
				if err := service.conn.Send(reply); err != nil {
					log.Println("Could not send reply:", err)
				}
			} else {
				service.outMessage <- &outMessage
			}
		default:
			log.Println("Received unknown method call on", msg.Interface, msg.Member)
			reply = dbus.NewErrorMessage(
				msg,
				"org.freedesktop.DBus.Error.UnknownMethod",
				fmt.Sprintf("No such method '%s' at object path '%s'", msg.Member, msg.Path),
			)
			if err := service.conn.Send(reply); err != nil {
				log.Println("Could not send reply:", err)
			}
		}
	}
}

func getUUIDFromObjectPath(objectPath dbus.ObjectPath) (string, error) {
	str := string(objectPath)
	defaultError := fmt.Errorf("%s is not a proper object path for a Message", str)
	if str == "" {
		return "", defaultError
	}
	uuid := filepath.Base(str)
	if uuid == "" || uuid == ".." || uuid == "." {
		return "", defaultError
	}
	return uuid, nil
}

func (service *MMSService) SetPreferredContext(context dbus.ObjectPath) error {
	// make set a noop if we are setting the same thing
	if pc, err := service.GetPreferredContext(); err != nil && context == pc {
		return nil
	}

	if err := storage.SetPreferredContext(service.identity, context); err != nil {
		return err
	}
	signal := dbus.NewSignalMessage(service.payload.Path, MMS_SERVICE_DBUS_IFACE, propertyChangedSignal)
	if err := signal.AppendArgs(preferredContextProperty, dbus.Variant{context}); err != nil {
		return err
	}
	return service.conn.Send(signal)
}

func (service *MMSService) GetPreferredContext() (dbus.ObjectPath, error) {
	return storage.GetPreferredContext(service.identity)
}

func (service *MMSService) setProperty(msg *dbus.Message) error {
	var propertyName string
	var propertyValue dbus.Variant
	if err := msg.Args(&propertyName, &propertyValue); err != nil {
		return err
	}

	switch propertyName {
	case preferredContextProperty:
		preferredContextObjectPath := dbus.ObjectPath(reflect.ValueOf(propertyValue.Value).String())
		service.Properties[preferredContextProperty] = dbus.Variant{preferredContextObjectPath}
		return service.SetPreferredContext(preferredContextObjectPath)
	default:
		errors.New("property cannot be set")
	}
	return errors.New("unhandled property")
}

// MessageRemoved closes message handlers, removes message from storage and emits the MessageRemoved signal to mms service dbus interface for message identified by objectPath parameter in this order.
// If message is not handled, removing from storage or sending signal fails, error is returned.
func (service *MMSService) MessageRemoved(objectPath dbus.ObjectPath) error {
	if service == nil {
		return ErrorNilMMSService
	}

	if _, ok := service.messageHandlers[objectPath]; !ok {
		return fmt.Errorf("message not handled")
	}

	service.messageHandlers[objectPath].Close()
	delete(service.messageHandlers, objectPath)

	uuid, err := getUUIDFromObjectPath(objectPath)
	if err != nil {
		return err
	}
	if err := storage.Destroy(uuid); err != nil {
		return err
	}

	return service.SingnalMessageRemoved(objectPath)
}

// Sends messageRemovedSignal signal to MMS_SERVICE_DBUS_IFACE to indicate, that the message stopped being handled and was removed from nuntium storage.
func (service *MMSService) SingnalMessageRemoved(objectPath dbus.ObjectPath) error {
	if service == nil {
		return ErrorNilMMSService
	}

	signal := dbus.NewSignalMessage(service.payload.Path, MMS_SERVICE_DBUS_IFACE, messageRemovedSignal)
	if err := signal.AppendArgs(objectPath); err != nil {
		return err
	}
	if err := service.conn.Send(signal); err != nil {
		return err
	}
	return nil
}

func (service *MMSService) IncomingMessageFailAdded(mNotificationInd *mms.MNotificationInd, downloadError error) error {
	if service == nil {
		return fmt.Errorf("Nil MMSService")
	}

	if err := mNotificationInd.PopDebugError(mms.DebugErrorTelepathyErrorNotify); err != nil {
		log.Printf("Forcing IncomingMessageFailAdded debug error: %#v", err)
		storage.UpdateMNotificationInd(mNotificationInd)
		return err
	}

	params := make(map[string]dbus.Variant)

	params["Status"] = dbus.Variant{"received"}
	params["Date"] = dbus.Variant{time.Now().Format(time.RFC3339)}
	params["Sender"] = dbus.Variant{strings.TrimSuffix(mNotificationInd.From, PLMN)}

	errorCode := "x-ubports-nuntium-mms-error-unknown"
	if eci, ok := downloadError.(interface{ Code() string }); ok {
		errorCode = eci.Code()
	}

	allowRedownload := false
	if ari, ok := downloadError.(interface{ AllowRedownload() bool }); ok {
		allowRedownload = ari.AllowRedownload()
	}

	expire := mNotificationInd.Expire().Format(time.RFC3339)
	if allowRedownload && mNotificationInd.Expired() {
		// Expired, don't allow redownload.
		log.Printf("Message expired at %s", mNotificationInd.Expire())
		allowRedownload = false
	}

	var mobileData *bool
	if enabled, err := service.MobileDataEnabled(); err == nil {
		mobileData = &enabled
	} else {
		log.Printf("Error detecting if mobile data is enabled: %v", err)
	}

	errorMessage, err := json.Marshal(&struct {
		Code       string
		Message    string
		Expire     string `json:",omitempty"`
		Size       uint64 `json:",omitempty"`
		MobileData *bool  `json:",omitempty"`
	}{errorCode, downloadError.Error(), expire, mNotificationInd.Size, mobileData})
	if err != nil {
		log.Printf("Error marshaling download error message to json: %v", err)
		errorMessage = []byte("{}")
	}
	params["Error"] = dbus.Variant{string(errorMessage)}
	params["AllowRedownload"] = dbus.Variant{allowRedownload}

	if mNotificationInd.RedownloadOfUUID != "" {
		params["DeleteEvent"] = dbus.Variant{string(service.GenMessagePath(mNotificationInd.RedownloadOfUUID))}
	}
	if !mNotificationInd.Received.IsZero() {
		params["Received"] = dbus.Variant{uint32(mNotificationInd.Received.Unix())}
	}

	payload := Payload{Path: service.GenMessagePath(mNotificationInd.UUID), Properties: params}

	// Don't pass a redownload channel to NewMessageInterface if redownload not allowed.
	redownloadChan := service.msgRedownloadChan
	if !allowRedownload {
		redownloadChan = nil
	}
	service.messageHandlers[payload.Path] = NewMessageInterface(service.conn, payload.Path, service.msgDeleteChan, redownloadChan)
	return service.MessageAdded(&payload)
}

//IncomingMessageAdded emits a MessageAdded with the path to the added message which
//is taken as a parameter and creates an object path on the message interface.
func (service *MMSService) IncomingMessageAdded(mRetConf *mms.MRetrieveConf, mNotificationInd *mms.MNotificationInd) error {
	if service == nil {
		return ErrorNilMMSService
	}

	if err := mNotificationInd.PopDebugError(mms.DebugErrorReceiveHandle); err != nil {
		log.Printf("Forcing getAndHandleMRetrieveConf debug error: %#v", err)
		storage.UpdateMNotificationInd(mNotificationInd)
		return err
	}

	payload, err := service.parseMessage(mRetConf)
	if err != nil {
		return err
	}

	if mNotificationInd.RedownloadOfUUID != "" {
		payload.Properties["DeleteEvent"] = dbus.Variant{string(service.GenMessagePath(mNotificationInd.RedownloadOfUUID))}
	}
	if !mNotificationInd.Received.IsZero() {
		payload.Properties["Received"] = dbus.Variant{mNotificationInd.Received.Unix()}
	}

	service.messageHandlers[payload.Path] = NewMessageInterface(service.conn, payload.Path, service.msgDeleteChan, nil)
	return service.MessageAdded(&payload)
}

func (service *MMSService) InitializationMessageAdded(mRetConf *mms.MRetrieveConf, mNotificationInd *mms.MNotificationInd) error {
	if service == nil {
		return ErrorNilMMSService
	}

	if mNotificationInd == nil {
		return ErrorNilMNotificationInd
	}

	path := service.GenMessagePath(mNotificationInd.UUID)
	if _, ok := service.messageHandlers[path]; ok {
		return fmt.Errorf("message is already handled")
	}

	// Initialization message only needs these properties to spawn proper handles in telepathy.
	payload := Payload{Path: path, Properties: map[string]dbus.Variant{
		"Status":  dbus.Variant{"received"},
		"Sender":  dbus.Variant{strings.TrimSuffix(mNotificationInd.From, PLMN)},
		"Rescued": dbus.Variant{true},
		"Silent":  dbus.Variant{true},
	}}

	// Extract "Sender" and "Recipients" property from mRetConf, if any.
	if mRetConf != nil {
		if pl, err := service.parseMessage(mRetConf); err == nil {
			if _, ok := pl.Properties["Sender"]; !ok {
				payload.Properties["Sender"] = pl.Properties["Sender"]
			}
			if _, ok := pl.Properties["Recipients"]; !ok {
				payload.Properties["Recipients"] = pl.Properties["Recipients"]
			}
		} else {
			log.Printf("Error parsing mRetConf for initialization message %s: %v", path, err)
		}
	}

	service.messageHandlers[path] = NewMessageInterface(service.conn, path, service.msgDeleteChan, service.msgRedownloadChan)
	return service.MessageAdded(&payload)
}

//MessageAdded emits a MessageAdded with the path to the added message which
//is taken as a parameter
func (service *MMSService) MessageAdded(msgPayload *Payload) error {
	signal := dbus.NewSignalMessage(service.payload.Path, MMS_SERVICE_DBUS_IFACE, messageAddedSignal)
	if err := signal.AppendArgs(msgPayload.Path, msgPayload.Properties); err != nil {
		return err
	}
	return service.conn.Send(signal)
}

func (service *MMSService) isService(identity string) bool {
	path := dbus.ObjectPath(MMS_DBUS_PATH + "/" + identity)
	if path == service.payload.Path {
		return true
	}
	return false
}

func (service *MMSService) Close() {
	service.conn.UnregisterObjectPath(service.payload.Path)
	close(service.msgChan)
	close(service.msgDeleteChan)
	close(service.msgRedownloadChan)
}

func (service *MMSService) parseMessage(mRetConf *mms.MRetrieveConf) (Payload, error) {
	params := make(map[string]dbus.Variant)
	params["Status"] = dbus.Variant{"received"}
	//TODO retrieve date correctly
	params["Date"] = dbus.Variant{parseDate(mRetConf.Date)}
	params["Sender"] = dbus.Variant{strings.TrimSuffix(mRetConf.From, PLMN)}
	if mRetConf.Subject != "" {
		params["Subject"] = dbus.Variant{mRetConf.Subject}
	}

	params["Recipients"] = dbus.Variant{parseRecipients(strings.Join(mRetConf.To, ","))}
	if smil, err := mRetConf.GetSmil(); err == nil {
		params["Smil"] = dbus.Variant{smil}
	}
	var attachments []Attachment
	dataParts := mRetConf.GetDataParts()
	for i := range dataParts {
		var filePath string
		if f, err := storage.GetMMS(mRetConf.UUID); err == nil {
			filePath = f
		} else {
			return Payload{}, err
		}
		attachment := Attachment{
			Id:        dataParts[i].ContentId,
			MediaType: dataParts[i].MediaType,
			FilePath:  filePath,
			Offset:    uint64(dataParts[i].Offset),
			Length:    uint64(len(dataParts[i].Data)),
		}
		attachments = append(attachments, attachment)
	}
	params["Attachments"] = dbus.Variant{attachments}
	payload := Payload{Path: service.GenMessagePath(mRetConf.UUID), Properties: params}
	return payload, nil
}

func parseDate(unixTime uint64) string {
	const layout = "2014-03-30T18:15:30-0300"
	date := time.Unix(int64(unixTime), 0)
	return date.Format(time.RFC3339)
}

func parseRecipients(to string) []string {
	recipients := strings.Split(to, ",")
	for i := range recipients {
		if strings.HasSuffix(recipients[i], PLMN) {
			recipients[i] = recipients[i][:len(recipients[i])-len(PLMN)]
		}
	}
	return recipients
}

func (service *MMSService) MessageDestroy(uuid string) error {
	msgObjectPath := service.GenMessagePath(uuid)
	if msgInterface, ok := service.messageHandlers[msgObjectPath]; ok {
		msgInterface.Close()
		delete(service.messageHandlers, msgObjectPath)
		return nil
	}
	return fmt.Errorf("no message interface handler for object path %s", msgObjectPath)
}

func (service *MMSService) MessageStatusChanged(uuid, status string) error {
	msgObjectPath := service.GenMessagePath(uuid)
	if msgInterface, ok := service.messageHandlers[msgObjectPath]; ok {
		return msgInterface.StatusChanged(status)
	}
	return fmt.Errorf("no message interface handler for object path %s", msgObjectPath)
}

func (service *MMSService) ReplySendMessage(reply *dbus.Message, uuid string) (dbus.ObjectPath, error) {
	msgObjectPath := service.GenMessagePath(uuid)
	reply.AppendArgs(msgObjectPath)
	if err := service.conn.Send(reply); err != nil {
		return "", err
	}
	msg := NewMessageInterface(service.conn, msgObjectPath, service.msgDeleteChan, nil)
	service.messageHandlers[msgObjectPath] = msg
	service.MessageAdded(msg.GetPayload())
	return msgObjectPath, nil
}

//TODO randomly creating a uuid until the download manager does this for us
func (service *MMSService) GenMessagePath(uuid string) dbus.ObjectPath {
	if service == nil {
		return dbus.ObjectPath(MMS_DBUS_PATH + "//" + uuid)
	}

	return dbus.ObjectPath(MMS_DBUS_PATH + "/" + service.identity + "/" + uuid)
}

// Returns if mobile data is enabled right now.
// Under the hood, DBus service property is read, if something fails, error is returned.
//
// dbus-send --session --print-reply \
//     --dest=com.ubuntu.connectivity1 \
//     /com/ubuntu/connectivity1/Private \
//     org.freedesktop.DBus.Properties.Get \
// string:com.ubuntu.connectivity1.Private \
// 	string:'MobileDataEnabled'
func (service *MMSService) MobileDataEnabled() (bool, error) {
	call := dbus.NewMethodCallMessage("com.ubuntu.connectivity1", "/com/ubuntu/connectivity1/Private", "org.freedesktop.DBus.Properties", "Get")
	call.AppendArgs("com.ubuntu.connectivity1.Private", "MobileDataEnabled")
	reply, err := service.conn.SendWithReply(call)
	if err != nil {
		return false, fmt.Errorf("send with reply error: %w", err)
	}
	if reply.Type == dbus.TypeError {
		return false, fmt.Errorf("reply is error: %w", reply.AsError())
	}

	var msg dbus.Variant
	if err := reply.Args(&msg); err != nil {
		return false, fmt.Errorf("reply decoding error: %w", err)
	}

	enabled, ok := msg.Value.(bool)
	if !ok {
		return false, fmt.Errorf("decoded variant does not contain bool vale: %#v", msg)
	}
	return enabled, nil
}

func (service *MMSService) HistoryService() *history.HistoryService {
	if service == nil {
		return nil
	}
	return history.NewHistoryService(service.conn)
}
