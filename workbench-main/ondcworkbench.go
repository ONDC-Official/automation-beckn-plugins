package ondcworkbench

import (
	"context"
	"fmt"
	"net/http"
	"ondcworkbench/internal/apiservice"
	"ondcworkbench/internal/ondc/cache/sessioncache"
	"ondcworkbench/internal/ondc/cache/subscribercache"
	"ondcworkbench/internal/ondc/cache/transactioncache"
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
	KeyManager definition.KeyManager
	requestReceiver *receiver.WorkbenchRequestReceiver
	TransactionCache *transactioncache.Service
	SessionCache	*sessioncache.Service
	SubscriberCache  *subscribercache.Service
}

func New(ctx context.Context, cache definition.Cache, keyMgr definition.KeyManager, config *Config) (definition.OndcWorkbench,func() error, error) {
	if(cache == nil){
		return nil, nil, fmt.Errorf("cache cannot be nil")
	}
	if(keyMgr == nil){
		return nil, nil, fmt.Errorf("key manager cannot be nil")
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
	log.Infof(ctx,"transaction properties loaded from config service: %#+v",config.TransactionProperties)
	
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

	return &ondcWorkbench{
		Config:    config,
		TransactionCache: txnCache,
		SessionCache: sessionCache,
		SubscriberCache: subscriberCache,
		KeyManager: keyMgr,
		requestReceiver:  rcv,
	}, nil, nil
}

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

// workbenchProcessor
func (w *ondcWorkbench) WorkbenchProcessor(ctx context.Context,request *http.Request,body []byte) (error){

	// print cookies for debugging
	for _, cookie := range request.Cookies() {
		log.Infof(context.Background(),"Cookie: %s=%s", cookie.Name, cookie.Value)
	}

	return nil
}