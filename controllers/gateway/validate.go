package gateway

import (
	"fmt"
	"github.com/argoproj/argo-events/pkg/apis/gateway/v1alpha1"
)

// Validates the gateway resource
func (goc *gwOperationCtx) validate() error {
	if goc.gw.Spec.DeploySpec == nil {
		return fmt.Errorf("gateway deploy specification is not specified")
	}
	if goc.gw.Spec.Type == "" {
		return fmt.Errorf("gateway type is not specified")
	}
	if goc.gw.Spec.Version == "" {
		return fmt.Errorf("gateway version is not specified")
	}
	switch goc.gw.Spec.DispatchMechanism {
	case v1alpha1.HTTPGateway:
		if goc.gw.Spec.Watchers == nil || (goc.gw.Spec.Watchers.Gateways == nil && goc.gw.Spec.Watchers.Sensors == nil) {
			return fmt.Errorf("no associated watchers with gateway")
		}
	case v1alpha1.NATSGateway:
	case v1alpha1.KafkaGateway:
	default:
		return fmt.Errorf("unknown gateway type")
	}
	return nil
}
