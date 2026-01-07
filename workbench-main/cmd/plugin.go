package main

import (
	"context"
	"fmt"
	"ondcworkbench"
	"ondcworkbench/internal/apiservice"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type workbenchProvider struct{}

func (w *workbenchProvider) New(ctx context.Context,cache definition.Cache,config map[string]string) (definition.OndcWorkbench, func() error, error){
	if(cache == nil){
		return nil,nil, fmt.Errorf("cache instance cannot be nil")
	}	
	auditURL := config["auditURL"]
	protocolVersion := config["protocolVersion"]
	protocolDomain := config["protocolDomain"]
	ModuleRole := config["moduleRole"]

	if(ModuleRole != "BAP" && ModuleRole != "BPP"){
		return nil,nil, fmt.Errorf("invalid moduleRole '%s'. Allowed values are 'BAP' or 'BPP'",ModuleRole)
	}

	ModuleType := config["moduleType"]

	if(ModuleType != (string)(apiservice.Caller) && ModuleType != (string)(apiservice.Receiver)){
		return nil,nil, fmt.Errorf("invalid moduleType '%s'. Allowed values are '%s' or '%s'",ModuleType, apiservice.Caller,apiservice.Receiver)
	}

	configServiceURL := config["configServiceURL"]

	if(configServiceURL == ""){
		return nil,nil, fmt.Errorf("configServiceURL cannot be empty")
	}

	return ondcworkbench.New(ctx,cache,&ondcworkbench.Config{
		AuditURL: auditURL,
		ProtocolVersion: protocolVersion,
		ProtocolDomain: protocolDomain,
		ModuleRole: ModuleRole,
		ModuleType: ModuleType,
		ConfigServiceURL: configServiceURL,
	})
}
// Provider is the exported provider instance for the Workbench plugin
var Provider = workbenchProvider{}