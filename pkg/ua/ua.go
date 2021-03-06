package ua

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/cloudwebrtc/go-sip-ua/pkg/account"
	"github.com/cloudwebrtc/go-sip-ua/pkg/auth"
	"github.com/cloudwebrtc/go-sip-ua/pkg/endpoint"
	"github.com/cloudwebrtc/go-sip-ua/pkg/invite"

	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/transaction"
	"github.com/ghettovoice/gosip/util"
)

type UserAgentConfig struct {
	UserAgent string
	Endpoint  *endpoint.EndPoint
	log       log.Logger
}

//UserAgent .
type UserAgent struct {
	InviteStateHandler   invite.InviteSessionHandler
	RegisterStateHandler account.RegisterHandler
	config               *UserAgentConfig
	iss                  sync.Map
	log                  log.Logger
}

//NewUserAgent .
func NewUserAgent(config *UserAgentConfig, logger log.Logger) *UserAgent {
	ua := &UserAgent{
		config:               config,
		iss:                  sync.Map{},
		InviteStateHandler:   nil,
		RegisterStateHandler: nil,
		log:                  logger.WithPrefix("UserAgent"),
	}
	endpoint := config.Endpoint
	endpoint.OnRequest(sip.INVITE, ua.handleInvite)
	endpoint.OnRequest(sip.ACK, ua.handleACK)
	endpoint.OnRequest(sip.BYE, ua.handleBye)
	endpoint.OnRequest(sip.CANCEL, ua.handleCancel)
	return ua
}

func (ua *UserAgent) handleInviteState(is *invite.Session, request *sip.Request, response *sip.Response, state invite.Status, tx *sip.Transaction) {
	if request != nil && *request != nil {
		is.StoreRequest(*request)
	}

	if response != nil && *response != nil {
		is.StoreResponse(*response)
	}

	if tx != nil {
		is.StoreTransaction(*tx)
	}

	is.SetState(state)

	if ua.InviteStateHandler != nil {
		ua.InviteStateHandler(is, request, response, state)
	}
}

func (ua *UserAgent) BuildRequest(
	method sip.RequestMethod,
	from *sip.Address,
	to *sip.Address,
	contact *sip.Address,
	target sip.SipUri,
	callID *sip.CallID) (*sip.Request, error) {

	logger := ua.log
	builder := sip.NewRequestBuilder().SetMethod(method).SetFrom(from).SetTo(to).SetRecipient(target.Clone()).AddVia(ua.buildViaHopHeader(target))

	if callID != nil {
		builder.SetCallID(callID)
	}
	builder.SetContact(contact)

	userAgent := sip.UserAgentHeader(ua.config.UserAgent)
	builder.SetUserAgent(&userAgent)

	req, err := builder.Build()
	if err != nil {
		logger.Errorf("err => %v", err)
		return nil, err
	}

	logger.Infof("buildRequest %s => %v", method, req)
	return &req, nil
}

func buildFrom(target sip.SipUri, user string, displayName string) *sip.Address {
	return &sip.Address{
		DisplayName: sip.String{Str: displayName},
		Uri: &sip.SipUri{
			FUser: sip.String{Str: user},
			FHost: target.Host(),
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: util.RandString(8)}),
	}
}

func buildTo(target sip.SipUri) *sip.Address {
	return &sip.Address{
		Uri: &sip.SipUri{
			FIsEncrypted: target.IsEncrypted(),
			FUser:        target.User(),
			FHost:        target.Host(),
		},
	}
}

func (ua *UserAgent) buildViaHopHeader(target sip.SipUri) *sip.ViaHop {
	protocol := "udp"
	if nt, ok := target.UriParams().Get("transport"); ok {
		protocol = nt.String()
	}
	e := ua.config.Endpoint
	netinfo := e.GetNetworkInfo(protocol)

	var host string = netinfo.Host
	if net.ParseIP(target.Host()).IsLoopback() {
		host = "127.0.0.1"
	}

	viaHop := &sip.ViaHop{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       protocol,
		Host:            host,
		Port:            netinfo.Port,
		Params:          sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	}
	return viaHop
}

func (ua *UserAgent) buildContact(uri sip.SipUri, instanceID *string) *sip.Address {
	e := ua.config.Endpoint
	contact := &sip.Address{
		Uri: &sip.SipUri{
			FHost:      "0.0.0.0",
			FUriParams: uri.FUriParams,
		},
	}

	if instanceID != nil {
		contact.Params = sip.NewParams().Add("+sip.instance", sip.String{Str: *instanceID})
	}

	protocol := "udp"
	if nt, ok := uri.UriParams().Get("transport"); ok {
		protocol = nt.String()
	}

	target := e.GetNetworkInfo(protocol)

	var host string = target.Host
	if net.ParseIP(uri.Host()).IsLoopback() {
		host = "127.0.0.1"
	}

	if contact.Uri.Host() == "0.0.0.0" {
		contact.Uri.SetHost(host)
	}

	if contact.Uri.Port() == nil {
		contact.Uri.SetPort(target.Port)
	}
	return contact
}

func (ua *UserAgent) handleRegisterState(profile *account.Profile, resp sip.Response, err error) {
	logger := ua.log

	if err != nil {
		logger.Errorf("Request [%s] failed, err => %v", sip.REGISTER, err)
		if ua.RegisterStateHandler != nil {
			reqErr := err.(*sip.RequestError)
			regState := account.RegisterState{
				Account:    *profile,
				Response:   nil,
				StatusCode: sip.StatusCode(reqErr.Code),
				Reason:     reqErr.Reason,
				Expiration: 0,
			}
			ua.RegisterStateHandler(regState)
		}
	}
	if resp != nil {
		stateCode := resp.StatusCode()
		logger.Infof("%s resp %d => %s", sip.REGISTER, stateCode, resp.String())
		if ua.RegisterStateHandler != nil {
			var expires sip.Expires = 0
			hdrs := resp.GetHeaders("Expires")
			if len(hdrs) > 0 {
				expires = *(hdrs[0]).(*sip.Expires)
			}
			regState := account.RegisterState{
				Account:    *profile,
				Response:   resp,
				StatusCode: resp.StatusCode(),
				Reason:     resp.Reason(),
				Expiration: uint32(expires),
			}
			ua.RegisterStateHandler(regState)
		}
	}
}

func (ua *UserAgent) SendRegister(profile *account.Profile, target sip.SipUri, expires uint32) {
	logger := ua.log

	from := buildFrom(target, profile.User, profile.DisplayName)
	contact := ua.buildContact(target, &profile.InstanceID)

	to := buildTo(target)
	request, err := ua.BuildRequest(sip.REGISTER, from, to, contact, target, nil)
	if err != nil {
		logger.Errorf("Register: err = %v", err)
		return
	}
	expiresHeader := sip.Expires(expires)
	(*request).AppendHeader(&expiresHeader)

	var authorizer *auth.ClientAuthorizer = nil
	if profile.Auth != nil {
		authorizer = auth.NewClientAuthorizer(profile.Auth.AuthName, profile.Auth.Password)
	}
	resp, err := ua.RequestWithContext(context.TODO(), *request, authorizer)
	ua.handleRegisterState(profile, resp, err)
}

func (ua *UserAgent) Invite(profile *account.Profile, target sip.SipUri, body *string) (*invite.Session, error) {
	logger := ua.log

	from := buildFrom(target, profile.User, profile.DisplayName)
	contact := ua.buildContact(target, &profile.InstanceID)
	to := buildTo(target)

	request, err := ua.BuildRequest(sip.INVITE, from, to, contact, target, nil)
	if err != nil {
		logger.Errorf("INVITE: err = %v", err)
		return nil, err
	}

	if body != nil {
		(*request).SetBody(*body, true)
		contentType := sip.ContentType("application/sdp")
		(*request).AppendHeader(&contentType)
	}

	var authorizer *auth.ClientAuthorizer = nil
	if profile.Auth != nil {
		authorizer = auth.NewClientAuthorizer(profile.Auth.AuthName, profile.Auth.Password)
	}

	resp, err := ua.RequestWithContext(context.TODO(), *request, authorizer)
	if err != nil {
		logger.Errorf("INVITE: Request [INVITE] failed, err => %v", err)
	}
	if resp != nil {
		stateCode := resp.StatusCode()
		logger.Infof("INVITE: resp %d => %s", stateCode, resp.String())
	}

	callID, ok := (*request).CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			return v.(*invite.Session), nil
		}
	}

	return nil, fmt.Errorf("Invite session not found!")
}

func (ua *UserAgent) Request(req *sip.Request) {
	ua.config.Endpoint.RequestWithContext(context.TODO(), *req, nil)
}

func (ua *UserAgent) SendBye(profile *account.Profile, callID *sip.CallID, target sip.SipUri) {
	logger := ua.log

	from := buildFrom(target, profile.User, profile.DisplayName)
	contact := ua.buildContact(target, &profile.InstanceID)

	to := buildTo(target)
	request, err := ua.BuildRequest(sip.BYE, from, to, contact, target, callID)
	if err != nil {
		logger.Errorf("Register: err = %v", err)
		return
	}

	var authorizer *auth.ClientAuthorizer = nil
	if profile.Auth != nil {
		authorizer = auth.NewClientAuthorizer(profile.Auth.AuthName, profile.Auth.Password)
	}
	ua.config.Endpoint.RequestWithContext(context.TODO(), *request, authorizer)
}

func (ua *UserAgent) handleBye(request sip.Request, tx sip.ServerTransaction) {
	logger := ua.log

	logger.Infof("handleBye: Request => %s, body => %s", request.Short(), request.Body())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")

	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*invite.Session)
			ua.iss.Delete(*callID)
			var transaction sip.Transaction = tx.(sip.Transaction)
			ua.handleInviteState(is, &request, nil, invite.Terminated, &transaction)
		}
	}

	tx.Respond(response)
}

func (ua *UserAgent) handleCancel(request sip.Request, tx sip.ServerTransaction) {
	logger := ua.log
	logger.Infof("handleCancel: Request => %s, body => %s", request.Short(), request.Body())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")
	tx.Respond(response)

	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*invite.Session)
			ua.iss.Delete(*callID)
			var transaction sip.Transaction = tx.(sip.Transaction)
			ua.handleInviteState(is, &request, nil, invite.Failure, &transaction)
		}
	}
}

func (ua *UserAgent) handleACK(request sip.Request, tx sip.ServerTransaction) {
	logger := ua.log
	logger.Infof("handleACK => %s, body => %s", request.Short(), request.Body())
	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			// handle Ringing or Processing with sdp
			is := v.(*invite.Session)
			ua.handleInviteState(is, &request, nil, invite.Confirmed, nil)
		}
	}
}

func (ua *UserAgent) handleInvite(request sip.Request, tx sip.ServerTransaction) {
	logger := ua.log
	//logger.Infof("handleInvite => %s, body => %s", request.Short(), request.Body())

	callID, ok := request.CallID()
	if ok {
		var transaction sip.Transaction = tx.(sip.Transaction)
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*invite.Session)
			ua.handleInviteState(is, &request, nil, invite.ReInviteReceived, &transaction)
		} else {
			//uri := request.Recipient().(*sip.SipUri)
			//contact := ua.buildContact(*uri, nil)
			contact, _ := request.Contact()
			is := invite.NewInviteSession(ua.config.Endpoint, "UAS", contact, request, *callID, transaction, invite.Incoming)
			ua.iss.Store(*callID, is)
			ua.handleInviteState(is, &request, nil, invite.InviteReceived, &transaction)
		}
	}

	go func() {
		cancel := <-tx.Cancels()
		if cancel != nil {
			logger.Infof("cancel => %s, body => %s", cancel.Short(), cancel.Body())
			response := sip.NewResponseFromRequest(cancel.MessageID(), cancel, 200, "OK", "")
			if callID, ok := response.CallID(); ok {
				if v, found := ua.iss.Load(*callID); found {
					ua.iss.Delete(*callID)
					is := v.(*invite.Session)
					ua.handleInviteState(is, &request, &response, invite.Failure, nil)
				}
			}

			tx.Respond(response)
		}
	}()

	go func() {
		ack := <-tx.Acks()
		if ack != nil {
			logger.Infof("ack => %v", ack)
		}
	}()
}

// RequestWithContext .
func (ua *UserAgent) RequestWithContext(ctx context.Context, request sip.Request, authorizer sip.Authorizer) (sip.Response, error) {
	e := ua.config.Endpoint
	tx, err := e.Request(sip.CopyRequest(request))
	if err != nil {
		return nil, err
	}

	if request.Method() == sip.INVITE {
		callID, ok := request.CallID()
		if ok {
			var transaction sip.Transaction = tx.(sip.Transaction)
			if _, found := ua.iss.Load(*callID); !found {
				//uri := request.Recipient().(*sip.SipUri)
				//contact := ua.buildContact(*uri, nil)
				contact, _ := request.Contact()
				is := invite.NewInviteSession(ua.config.Endpoint, "UAC", contact, request, *callID, transaction, invite.Outgoing)
				ua.iss.Store(*callID, is)
				ua.handleInviteState(is, &request, nil, invite.InviteSent, &transaction)
			}
		}
	}

	responses := make(chan sip.Response)
	provisionals := make(chan sip.Response)
	errs := make(chan error)
	go func() {
		var lastResponse sip.Response

		previousResponses := make([]sip.Response, 0)
		previousResponsesStatuses := make(map[sip.StatusCode]bool)

		for {
			select {
			case <-ctx.Done():
				if lastResponse != nil && lastResponse.IsProvisional() {
					e.CancelRequest(request, lastResponse)
				}
				if lastResponse != nil {
					lastResponse.SetPrevious(previousResponses)
				}
				errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
				// pull out later possible transaction responses and errors
				go func() {
					for {
						select {
						case <-tx.Done():
							return
						case <-tx.Errors():
						case <-tx.Responses():
						}
					}
				}()
				return
			case err, ok := <-tx.Errors():
				if !ok {
					if lastResponse != nil {
						lastResponse.SetPrevious(previousResponses)
					}
					errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
					return
				} else {
					switch err.(type) {
					case *transaction.TxTimeoutError:
						{
							errs <- sip.NewRequestError(408, "Request Timeout", request, lastResponse)
							return
						}
						break
					}
				}
				//errs <- err
				return
			case response, ok := <-tx.Responses():
				if !ok {
					if lastResponse != nil {
						lastResponse.SetPrevious(previousResponses)
					}
					errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
					return
				}

				response = sip.CopyResponse(response)
				lastResponse = response

				if response.IsProvisional() {
					if _, ok := previousResponsesStatuses[response.StatusCode()]; !ok {
						previousResponses = append(previousResponses, response)
					}
					provisionals <- response
					continue
				}

				// success
				if response.IsSuccess() {
					response.SetPrevious(previousResponses)

					if request.IsInvite() {
						e.AckInviteRequest(request, response)
						e.RememberInviteRequest(request)
						go func() {
							for response := range tx.Responses() {
								e.AckInviteRequest(request, response)
							}
						}()
					}
					responses <- response
					tx.Done()
					return
				}

				// unauth request
				if (response.StatusCode() == 401 || response.StatusCode() == 407) && authorizer != nil {
					if err := authorizer.AuthorizeRequest(request, response); err != nil {
						errs <- err
						return
					}
					if response, err := ua.config.Endpoint.RequestWithContext(ctx, request, nil); err == nil {
						responses <- response
					} else {
						errs <- err
					}
					return
				}

				// failed request
				if lastResponse != nil {
					lastResponse.SetPrevious(previousResponses)
				}
				errs <- sip.NewRequestError(uint(response.StatusCode()), response.Reason(), request, lastResponse)
				return
			}
		}
	}()

	waitForResult := func() (sip.Response, error) {
		for {
			select {
			case provisional := <-provisionals:
				callID, ok := provisional.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						is := v.(*invite.Session)
						is.StoreResponse(provisional)
						// handle Ringing or Processing with sdp
						ua.handleInviteState(is, &request, &provisional, invite.Provisional, nil)
						if len(provisional.Body()) > 0 {
							ua.handleInviteState(is, &request, &provisional, invite.EarlyMedia, nil)
						}
					}
				}
			case err := <-errs:
				//TODO: error type switch transaction.TxTimeoutError
				switch err.(type) {
				case *transaction.TxTimeoutError:
					//errs <- sip.NewRequestError(408, "Request Timeout", nil, nil)
					return nil, err
				}
				request := (err.(*sip.RequestError)).Request
				response := (err.(*sip.RequestError)).Response
				callID, ok := request.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						is := v.(*invite.Session)
						ua.iss.Delete(*callID)
						// handle Ringing or Processing with sdp
						ua.handleInviteState(is, &request, &response, invite.Failure, nil)
					}
				}
				return nil, err
			case response := <-responses:
				callID, ok := response.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						// handle Ringing or Processing with sdp
						is := v.(*invite.Session)
						ua.handleInviteState(is, &request, &response, invite.Confirmed, nil)
					}
				}
				return response, nil
			}
		}
	}

	if request.IsInvite() {
		go waitForResult()
		return nil, err
	}
	return waitForResult()
}

func (ua *UserAgent) RemoveSess(callID *sip.CallID) {
	ua.iss.Delete(*callID)
}

func (ua *UserAgent) Shutdown() {
	ua.config.Endpoint.Shutdown()
}
