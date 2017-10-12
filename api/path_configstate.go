package api

import (
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/cutil"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/persistence"
	"github.com/open-horizon/anax/policy"
	"strings"
)

const CONFIGSTATE_CONFIGURING = "configuring"
const CONFIGSTATE_CONFIGURED = "configured"

func NoOpStateChange(from string, to string) bool {
	if from == to {
		return true
	}
	return false
}

func ValidStateChange(from string, to string) bool {
	if from == CONFIGSTATE_CONFIGURING && to == CONFIGSTATE_CONFIGURED {
		return true
	}
	return false
}

func FindConfigstateForOutput(db *bolt.DB) (*Configstate, error) {

	var device *HorizonDevice

	pDevice, err := persistence.FindExchangeDevice(db)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("unable to read horizondevice object, error %v", err))
	} else if pDevice == nil {
		state := CONFIGSTATE_CONFIGURING
		cfg := &Configstate{
			State: &state,
		}
		return cfg, nil

	} else {
		device = ConvertFromPersistentHorizonDevice(pDevice)
		return device.Config, nil
	}

}

// Given a demarshalled Configstate object, validate it and save, returning any errors.
func UpdateConfigstate(cfg *Configstate,
	errorhandler ErrorHandler,
	getOrg OrgHandler,
	getMicroservice MicroserviceHandler,
	getPatterns PatternHandler,
	resolveWorkload WorkloadResolverHandler,
	db *bolt.DB,
	config *config.HorizonConfig) (bool, *Configstate, []*events.PolicyCreatedMessage) {

	// Check for the device in the local database. If there are errors, they will be written
	// to the HTTP response.
	pDevice, err := persistence.FindExchangeDevice(db)
	if err != nil {
		return errorhandler(NewSystemError(fmt.Sprintf("Unable to read horizondevice object, error %v", err))), nil, nil
	} else if pDevice == nil {
		return errorhandler(NewNotFoundError("Exchange registration not recorded. Complete account and device registration with an exchange and then record device registration using this API's /horizondevice path.")), nil, nil
	}

	glog.V(3).Infof(apiLogString(fmt.Sprintf("Update configstate: device in local database: %v", pDevice)))
	msgs := make([]*events.PolicyCreatedMessage, 0, 10)

	// Device registration is in the database, so verify that the requested state change is suported.
	// The only (valid) state transition that is currently unsupported is configured to configuring.
	// If the caller is requesting a state change that is a noop, just return the current state.
	if *cfg.State != CONFIGSTATE_CONFIGURING && *cfg.State != CONFIGSTATE_CONFIGURED {
		return errorhandler(NewAPIUserInputError(fmt.Sprintf("Supported state values are '%v' and '%v'.", CONFIGSTATE_CONFIGURING, CONFIGSTATE_CONFIGURED), "configstate.state")), nil, nil
	} else if NoOpStateChange(pDevice.Config.State, *cfg.State) {
		exDev := ConvertFromPersistentHorizonDevice(pDevice)
		return false, exDev.Config, nil
	} else if !ValidStateChange(pDevice.Config.State, *cfg.State) {
		return errorhandler(NewAPIUserInputError(fmt.Sprintf("Transition from '%v' to '%v' is not supported.", pDevice.Config.State, *cfg.State), "configstate.state")), nil, nil
	}

	// From the node's pattern, resolve all the workloads to microservices and then register each microservice that is not already registered.
	if pDevice.Pattern != "" {

		glog.V(3).Infof(apiLogString(fmt.Sprintf("Configstate autoconfig of microservices starting")))

		// Get the pattern definition from the exchange. There should only be one pattern returned in the map.
		pattern, err := getPatterns(pDevice.Org, pDevice.Pattern, pDevice.GetId(), pDevice.Token)
		if err != nil {
			return errorhandler(NewSystemError(fmt.Sprintf("Unable to read pattern object %v from exchange, error %v", pDevice.Pattern, err))), nil, nil
		} else if len(pattern) != 1 {
			return errorhandler(NewSystemError(fmt.Sprintf("Expected only 1 pattern from exchange, received %v", len(pattern)))), nil, nil
		}

		// Get the pattern definition that we need to analyze.
		patId := fmt.Sprintf("%v/%v", pDevice.Org, pDevice.Pattern)
		patternDef, ok := pattern[patId]
		if !ok {
			return errorhandler(NewSystemError(fmt.Sprintf("Expected pattern id not found in GET pattern response: %v", pattern))), nil, nil
		}

		glog.V(5).Infof(apiLogString(fmt.Sprintf("Configstate working with pattern definition %v", patternDef)))

		// For each workload in the pattern, resolve the workload to a list of required microservices.
		completeAPISpecList := new(policy.APISpecList)
		thisArch := cutil.ArchString()
		for _, workload := range patternDef.Workloads {

			// Ignore workloads that don't match this node's hardware architecture.
			if workload.WorkloadArch != thisArch {
				continue
			}

			// Each workload in the pattern can specify rollback workload versions, so to get a fully qualified workload URL,
			// we need to iterate each workload choice to grab the version.
			for _, workloadChoice := range workload.WorkloadVersions {
				apiSpecList, err := resolveWorkload(workload.WorkloadURL, workload.WorkloadOrg, workloadChoice.Version, thisArch, pDevice.GetId(), pDevice.Token)
				if err != nil {
					return errorhandler(NewSystemError(fmt.Sprintf("Error resolving workload %v %v %v %v, error %v", workload.WorkloadURL, workload.WorkloadOrg, workloadChoice.Version, thisArch, err))), nil, nil
				}

				// Microservices that are defined as being shared singletons can only appear once in the complete API spec list. If there
				// are 2 versions of the same shared singleton microservice, the higher version of the 2 will be auto configured.
				completeAPISpecList.ReplaceHigherSharedSingleton(apiSpecList)

				// MergeWith will omit exact duplicates when merging the 2 lists.
				(*completeAPISpecList) = completeAPISpecList.MergeWith(apiSpecList)
			}

		}

		glog.V(5).Infof(apiLogString(fmt.Sprintf("Configstate resolved pattern to APISpecs %v", *completeAPISpecList)))

		// Using the list of APISpec objects, we can create a service (microservice) on this node automatically, for each microservice
		// that already has configuration or which doesnt need it.
		var createServiceError error
		passthruHandler := GetPassThroughErrorHandler(&createServiceError)
		for _, apiSpec := range *completeAPISpecList {

			service := NewService(apiSpec.SpecRef, apiSpec.Org, makeServiceName(apiSpec.SpecRef, apiSpec.Org, apiSpec.Version), apiSpec.Version)
			errHandled, newService, msg := CreateService(service, passthruHandler, getMicroservice, db, config)
			if errHandled {
				switch createServiceError.(type) {
				case *MSMissingVariableConfigError:
					msErr := err.(*MSMissingVariableConfigError)
					// Cannot autoconfig this microservice because it has variables that need to be configured.
					return errorhandler(NewAPIUserInputError(fmt.Sprintf("Configstate autoconfig, microservice %v %v %v, %v", apiSpec.SpecRef, apiSpec.Org, apiSpec.Version, msErr.Err), "configstate.state")), nil, nil

				case *DuplicateServiceError:
					// If the microservice is already registered, that's ok because the node user is allowed to configure any of the
					// required microservices before calling the configstate API.

				default:
					return errorhandler(NewSystemError(fmt.Sprintf("unexpected error returned from service create (%T) %v", createServiceError, createServiceError))), nil, nil
				}
			} else {
				glog.V(5).Infof(apiLogString(fmt.Sprintf("Configstate autoconfig created service %v", newService)))
				msgs = append(msgs, msg)
			}
		}

		glog.V(3).Infof(apiLogString(fmt.Sprintf("Configstate autoconfig of microservices complete")))

	}

	// Update the state in the local database
	updatedDev, err := pDevice.SetConfigstate(db, pDevice.Id, *cfg.State)
	if err != nil {
		return errorhandler(NewSystemError(fmt.Sprintf("error persisting new config state: %v", err))), nil, nil
	}

	glog.V(5).Infof(apiLogString(fmt.Sprintf("Update configstate: updated device: %v", updatedDev)))

	exDev := ConvertFromPersistentHorizonDevice(updatedDev)
	return false, exDev.Config, msgs

}

func makeServiceName(msURL string, msOrg string, msVersion string) string {

	url := ""
	pieces := strings.SplitN(msURL, "/", 3)
	if len(pieces) >= 3 {
		url = strings.TrimSuffix(pieces[2], "/")
		url = strings.Replace(url, "/", "-", -1)
	}

	return fmt.Sprintf("%v_%v_%v", url, msOrg, msVersion)

}