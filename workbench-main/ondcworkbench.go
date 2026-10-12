package ondcworkbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/sessioncache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/subscribercache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/transactioncache"
	contextvalidatotors "github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/contextValidatotors"

	"strings"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/payloadutils"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/receiver"
	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type Config struct {
	RegistryURL	            string
	ProtocolVersion           string
	ProtocolDomain            string
	ModuleRole                string // BAP or BPP
	ModuleType                string // caller or receiver
	ConfigServiceURL          string
	MockServiceURL            string
	TransactionPropertiesPath string // optional: load TransactionProperties from a JSON/YAML file instead of config service
	TransactionProperties     apiservice.TransactionProperties
}

type ondcWorkbench struct {
	Config *Config
	requestReceiver *receiver.WorkbenchRequestReceiver
	TransactionCache *transactioncache.Service
	SessionCache	*sessioncache.Service
	SubscriberCache  *subscribercache.Service
	ContextValidator *contextvalidatotors.Validator
}

func New(ctx context.Context, cache definition.Cache, config *Config) (definition.OndcWorkbench,func() error, error) {
	if(cache == nil){
		return nil, nil, fmt.Errorf("cache cannot be nil")
	}
	
	if(config == nil){
		return nil, nil, fmt.Errorf("config cannot be nil")
	}
	configErr := validateConfig(config)
	if(configErr != nil){
		return nil, nil, configErr
	}

	transactionProperties, txnPropErr := loadTransactionProperties(ctx, config)
	if txnPropErr != nil {
		return nil, nil, txnPropErr
	}

	config.TransactionProperties = transactionProperties
	log.Infof(ctx, "transaction properties loaded: %#+v", transactionProperties)
	
	txnCache := &transactioncache.Service{
		Cache: cache,
	}
	sessionCache := &sessioncache.Service{
		Cache: cache,
	}
	subscriberCache := &subscribercache.Service{
		Cache: cache,
	}
	rcv := receiver.NewWorkbenchRequestReceiver(*txnCache,*subscriberCache,*sessionCache)

	contextValidator := contextvalidatotors.NewContextValidator(txnCache,transactionProperties)

	return &ondcWorkbench{
		Config:    config,
		TransactionCache: txnCache,
		SessionCache: sessionCache,
		SubscriberCache: subscriberCache,
		requestReceiver:  rcv,
		ContextValidator: contextValidator,
	}, nil, nil
}
// WorkbenchReceiver
func (w *ondcWorkbench) WorkbenchReceiver(ctx context.Context, request *http.Request, body []byte) error {
	return payloadutils.AsWorkbenchError(w.receive(ctx, request, body))
}

func (w *ondcWorkbench) receive(ctx context.Context, request *http.Request,body []byte) (error){
	payloadEnv,payloadRaw, err := payloadutils.ValidateAndExtractPayload(ctx,body,w.Config.TransactionProperties)
	if(err != nil){
		log.Errorf(ctx,err,"payload receive failed")
		return err
	}
	requestOwner,subscriberURL,subscriberID,moduleRole, err := payloadutils.GetRequestData(
		payloadEnv,
		apiservice.ModuleType(w.Config.ModuleType),
		apiservice.ModuleRole(w.Config.ModuleRole),
		*request.URL)
	if(err != nil){
		log.Errorf(ctx,err,"payload receive failed")
		return err
	}
	workbenchRequestData := &apiservice.WorkbenchRequestData{
		Request:      request,
		BodyRaw:      payloadRaw,
		BodyEnvelope: payloadEnv,
		ModuleType: apiservice.ModuleType(w.Config.ModuleType),
		ModuleRole: apiservice.ModuleRole(moduleRole),
		RequestOwner: requestOwner,
		SubscriberURL: subscriberURL,
		SubscriberID: subscriberID,
		TransactionID: payloadEnv.Context.TransactionID,
		TransactionProperties: w.Config.TransactionProperties,
	}
	receiverErr :=  payloadutils.NewInternalServerNackError(
		fmt.Sprintf("unsupported module type: %s; only receiver or caller are supported", w.Config.ModuleType),
		workbenchRequestData.BodyRaw["context"],
	) 
	if(w.Config.ModuleType == string(apiservice.Receiver)){
		receiverErr = w.requestReceiver.ReceiveFromNP(context.Background(),workbenchRequestData)
	} else if(w.Config.ModuleType == string(apiservice.Caller)) {
		receiverErr = w.requestReceiver.ReceiveFromMock(context.Background(),workbenchRequestData)
	}
	if(receiverErr == nil){
		log.Infof(context.Background(),"payload received successfully for transaction ID: %s",workbenchRequestData.TransactionID)

		mockURL := ""
		if(workbenchRequestData.UsecaseID == "PLAYGROUND-FLOW"){
			mockURL = w.Config.MockServiceURL + "/playground/manual"
		}else{
			// check if MockServiceURL has localhost 
			if strings.Contains(w.Config.MockServiceURL,"localhost"){
				mockURL = fmt.Sprintf("%s/%s/manual",w.Config.MockServiceURL,w.Config.ProtocolDomain)
			}else{
				mockURL = fmt.Sprintf("%s/%s/%s/manual",w.Config.MockServiceURL,w.Config.ProtocolDomain,w.Config.ProtocolVersion)
			}
		}
		workbenchRequestData.MockURL = mockURL
		err:= setRequestCookies(workbenchRequestData)
		if(err != nil){
			log.Errorf(ctx,err,"failed to set request cookies for transaction ID: %s",workbenchRequestData.TransactionID)
			return err
		}
		err = w.createTransactionCache(ctx,workbenchRequestData)
		if(err != nil){
			log.Errorf(ctx,err,"failed to create transaction cache for transaction ID: %s",workbenchRequestData.TransactionID)
			return err
		}
	}
	return receiverErr
}

// WorkbenchValidateContext
func (w *ondcWorkbench) WorkbenchValidateContext(ctx context.Context, request *http.Request, body []byte) error {
	return payloadutils.AsWorkbenchError(w.validateContext(ctx, request, body))
}

func (w *ondcWorkbench) validateContext(ctx context.Context,request *http.Request,body []byte) (error){
	payloadEnv, raw, err := payloadutils.ValidateAndExtractPayload(ctx, body, w.Config.TransactionProperties)
	if err != nil {
		log.Errorf(ctx, err, "context validation: failed to parse payload")
		return err
	}
	err = w.ContextValidator.ValidateContext(ctx, request, payloadEnv, raw)
	if err != nil {
		log.Errorf(ctx, err, "context validation failed")
		return err
	}

	version:= payloadEnv.Context.Version
	if(version == "" ){
		version = payloadEnv.Context.CoreVersion
	}

	customResponse := map[string]interface{}{}
	if(strings.HasPrefix(version,"1")){
		customResponse = payloadutils.NewAckObject(raw["context"])
	}else{
		customResponse = payloadutils.NewAckObject(nil)
	}
	customBytes, _ := json.Marshal(customResponse)
	encoded := base64.StdEncoding.EncodeToString(customBytes)
	request.AddCookie(&http.Cookie{
		Name:  "custom-response-body",
		Value: encoded,
	})
	return nil
}

