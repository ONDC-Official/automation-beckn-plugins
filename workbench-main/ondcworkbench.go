package ondcworkbench

import (
	"context"
	"fmt"
	"net/http"
	"ondcworkbench/internal/apiservice"
	"ondcworkbench/internal/ondc/cache/sessioncache"
	"ondcworkbench/internal/ondc/cache/subscribercache"
	"ondcworkbench/internal/ondc/cache/transactioncache"
	contextvalidatotors "ondcworkbench/internal/ondc/contextValidatotors"
	"ondcworkbench/internal/ondc/payloadutils"
	"ondcworkbench/internal/receiver"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type Config struct {
	AuditURL 	 string
	ProtocolVersion   string
	ProtocolDomain   string
	ModuleRole string // BAP or BPP
	ModuleType string // caller or receiver
	ConfigServiceURL string
	TransactionProperties apiservice.TransactionProperties
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

	transactionProperties, txnPropErr := getTransactionPropertiesFromConfigService(ctx,config.ConfigServiceURL,config.ProtocolDomain,config.ProtocolVersion)

	if(txnPropErr != nil){
		return nil, nil, fmt.Errorf("failed to get transaction properties from config service: %v",txnPropErr)
	}

	config.TransactionProperties = transactionProperties
	log.Infof(ctx,"transaction properties loaded from config service: %#+v",transactionProperties)
	
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
func (w *ondcWorkbench) WorkbenchReceiver(ctx context.Context, request *http.Request,body []byte) (error){
	payloadEnv,payloadRaw, err := payloadutils.ValidateAndExtractPayload(ctx,body,w.Config.TransactionProperties)
	if(err != nil){
		log.Errorf(ctx,err,"payload receive failed")
		return err
	}
	requestOwner,subscriberURL, err := payloadutils.GetRequestData(payloadEnv,apiservice.ModuleType(w.Config.ModuleType),apiservice.ModuleRole(w.Config.ModuleRole),*request.URL)
	if(err != nil){
		log.Errorf(ctx,err,"payload receive failed")
		return err
	}
	workbenchRequestData := &apiservice.WorkbenchRequestData{
		Request:      request,
		BodyRaw:      payloadRaw,
		BodyEnvelope: payloadEnv,
		ModuleType: apiservice.ModuleType(w.Config.ModuleType),
		RequestOwner: requestOwner,
		SubscriberURL: subscriberURL,
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
func (w *ondcWorkbench) WorkbenchValidateContext(ctx context.Context,request *http.Request,body []byte) (error){
	payloadEnv, raw, err := payloadutils.ValidateAndExtractPayload(ctx, body, w.Config.TransactionProperties)
	if err != nil {
		log.Errorf(ctx, err, "context validation: failed to parse payload")
		return err
	}
	cleanUpHttpRequest(request)
	return w.ContextValidator.ValidateContext(ctx, request, payloadEnv, raw)
}

