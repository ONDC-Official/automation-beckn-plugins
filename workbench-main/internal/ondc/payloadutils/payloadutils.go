package payloadutils

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"ondcworkbench/internal/apiservice"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

func ValidateAndExtractPayload(ctx context.Context,body []byte,txnProperties apiservice.TransactionProperties) (apiservice.PayloadEnvelope,apiservice.PayloadRaw,error){
	var envelope apiservice.PayloadEnvelope 
	var bodyRaw apiservice.PayloadRaw
	if err := json.Unmarshal(body, &bodyRaw); err != nil {
		log.Errorf(ctx,err,"failed to unmarshal payload body")
		return apiservice.PayloadEnvelope{},nil, NewBadRequestHTTPError("failed to parse payload body")
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		log.Errorf(ctx,err,"failed to unmarshal payload envelope")
		return apiservice.PayloadEnvelope{},nil, NewBadRequestHTTPError("failed to parse payload envelope")
	}

	if(envelope.Context.TransactionID == ""){
		return apiservice.PayloadEnvelope{},nil, NewBadRequestNackError("transaction_id is missing in context",bodyRaw["context"])
	}

	allowedApis := make([]string, 0, len(txnProperties.APIProperties))
    for action := range txnProperties.APIProperties {
        allowedApis = append(allowedApis, action)
    }

	if(envelope.Context.Action == ""){
		return apiservice.PayloadEnvelope{},nil, NewBadRequestNackError("action is missing in context",bodyRaw["context"])
	} else {
		isValidAction := false
		for _, action := range allowedApis {
			if(envelope.Context.Action == action){
				isValidAction = true
				break
			}
		}
		if(!isValidAction){
			msg:= fmt.Sprintf("invalid action '%s' in context. Allowed actions are: %v",envelope.Context.Action,allowedApis)
			return apiservice.PayloadEnvelope{},nil, NewBadRequestNackError(msg,bodyRaw["context"])
		}
	}
	return envelope,bodyRaw,nil
}

func GetRequestData(payload apiservice.PayloadEnvelope, moduleType apiservice.ModuleType, moduleRole apiservice.ModuleRole, URL url.URL) (apiservice.RequestOwner, string,string,string, error) {
	if(moduleType == apiservice.Caller){
		action := payload.Context.Action
		if(strings.HasPrefix(action,"on_")) {
			moduleRole = apiservice.BPP
		}else{
			moduleRole = apiservice.BAP
		}
	}
	
	// Create a composite key for switch
	key := fmt.Sprintf("%s-%s", moduleType, moduleRole)
	
	var subscriberURI string
	var owner apiservice.RequestOwner
	var subscriberID string
	

	switch key {
	case fmt.Sprintf("%s-%s", apiservice.Receiver, apiservice.BAP):
		// Receiver BAP receives requests at bap_uri from BPP (Seller side)
		subscriberURI = payload.Context.BapURI // coutner party np to forward requests
		subscriberID = payload.Context.BapID // it our own ID for signing purposes
		if subscriberURI == "" {
			return "", "","", "", NewBadRequestNackError("bap_uri is missing in context", payload.Context)
		}
		owner = apiservice.SellerNP
		
	case fmt.Sprintf("%s-%s", apiservice.Receiver, apiservice.BPP):
		// Receiver BPP receives requests at bpp_uri from BAP (Buyer side)
		subscriberURI = payload.Context.BppURI
		subscriberID = payload.Context.BppID
		if subscriberURI == "" {
			return "", "", "", "", NewBadRequestNackError("bpp_uri is missing in context", payload.Context)
		}
		owner = apiservice.BuyerNP
		
	case fmt.Sprintf("%s-%s", apiservice.Caller, apiservice.BAP):
		// Caller BAP makes requests to BPP (Buyer side calling Seller)
		subscriberURI = payload.Context.BppURI
		subscriberID = payload.Context.BapID
		if subscriberURI == "" {
			// Fallback to query parameter
			subscriberURI = URL.Query().Get("subscriber_url")
			if subscriberURI == "" {
				return "", "", "","", NewBadRequestHTTPError("bpp_uri is missing in context and subscriber_url query param is also missing")
			}
		}
		owner = apiservice.BuyerNP
		
	case fmt.Sprintf("%s-%s", apiservice.Caller, apiservice.BPP):
		// Caller BPP makes requests to BAP (Seller side calling Buyer)
		subscriberURI = payload.Context.BapURI
		subscriberID = payload.Context.BppID
		if subscriberURI == "" {
			return "", "", "", "", NewBadRequestHTTPError("bap_uri is missing in context")
		}
		owner = apiservice.SellerNP
		
	default:
		return "", "", "", "", fmt.Errorf("unable to determine request owner and subscriber url for moduleType: %s and moduleRole: %s", moduleType, moduleRole)
	}
	
	return owner, subscriberURI, subscriberID,string(moduleRole), nil
}

func NewBadRequestNackError(msg string,ondcContext any) error {
	return model.NewWorkbenchErr("BAD_REQUEST",msg, "NACK",ondcContext)
}

func NewInternalServerNackError(msg string, ondcContext any) error {
	return model.NewWorkbenchErr("INTERNAL", msg, "NACK", ondcContext)
}

func NewBadRequestHTTPError(msg string) error {
	return model.NewWorkbenchErr("BAD_REQUEST",msg, "HTTP",nil)
}

func NewPreconditionFailedHTTPError(msg string) error {
	return model.NewWorkbenchErr("PRECONDITION_FAILED",msg, "HTTP",nil)
}