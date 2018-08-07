package gateway_controller

import (
	"github.com/argoproj/argo-events/pkg/apis/gateway/v1alpha1"
	client "github.com/argoproj/argo-events/pkg/gateway-client/clientset/versioned/typed/gateway/v1alpha1"
	zlog "github.com/rs/zerolog"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"

	"fmt"
	"github.com/argoproj/argo-events/common"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"strings"
)

// the context of an operation on a gateway-controller.
// the gateway-controller-controller creates this context each time it picks a Gateway off its queue.
type gwOperationCtx struct {
	// gw is the gateway-controller object
	gw *v1alpha1.Gateway
	// updated indicates whether the gateway-controller object was updated and needs to be persisted back to k8
	updated bool
	// log is the logger for a gateway
	log zlog.Logger
	// reference to the gateway-controller-controller
	controller *GatewayController
}

// newGatewayOperationCtx creates and initializes a new gOperationCtx object
func newGatewayOperationCtx(gw *v1alpha1.Gateway, controller *GatewayController) *gwOperationCtx {
	return &gwOperationCtx{
		gw:         gw.DeepCopy(),
		updated:    false,
		log:        zlog.New(os.Stdout).With().Str("name", gw.Name).Str("namespace", gw.Namespace).Logger(),
		controller: controller,
	}
}

func (goc *gwOperationCtx) operate() error {
	goc.log.Info().Msg("started operating on the gateway")
	// validate the gateway
	err := goc.validate()
	if err != nil {
		goc.log.Error().Err(err).Msg("gateway validation failed")
		return err
	}
	gatewayClient := goc.controller.gatewayClientset.ArgoprojV1alpha1().Gateways(goc.gw.Namespace)

	// manages states of a gateway
	switch goc.gw.Status {
	case v1alpha1.NodePhaseNew:
		// Update node phase to running
		goc.gw.Status = v1alpha1.NodePhaseRunning

		// declare the configuration map for gateway transformer
		gatewayConfigMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      common.DefaultGatewayTransformerConfigMapName(goc.gw.Name),
				Namespace: goc.gw.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(goc.gw, v1alpha1.SchemaGroupVersionKind),
				},
			},
			Data: map[string]string{
				common.EventSource:      goc.gw.Name,
				common.EventTypeVersion: goc.gw.Spec.Version,
				common.EventType:        goc.gw.Spec.Type,
				common.SensorList:       strings.Join(goc.gw.Spec.Sensors, ","),
			},
		}
		// create gateway transformer configmap
		_, err = goc.controller.kubeClientset.CoreV1().ConfigMaps(goc.gw.Namespace).Create(gatewayConfigMap)
		if err != nil {
			goc.log.Error().Err(err).Msg("failed to create transformer gateway configuration")
			// mark gateway as failed
			goc.gw.Status = v1alpha1.NodePhaseError
			goc.gw, err = gatewayClient.Update(goc.gw)
			if err != nil {
				err = goc.reapplyUpdate(gatewayClient)
				if err != nil {
					goc.log.Error().Err(err).Msg("failed to update gateway")
					return err
				}
			}
		}

		// set the image policy if not specified
		if goc.gw.Spec.ImagePullPolicy == "" {
			goc.gw.Spec.ImagePullPolicy = corev1.PullAlways
		}

		gatewayDeployment := &appv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      common.DefaultGatewayDeploymentName(goc.gw.Name),
				Namespace: goc.gw.Namespace,
				Labels: map[string]string{
					common.LabelGatewayName: goc.gw.Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(goc.gw, v1alpha1.SchemaGroupVersionKind),
				},
			},
			Spec: appv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						common.LabelGatewayName: goc.gw.Name,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							common.LabelGatewayName: goc.gw.Name,
						},
					},
					Spec: corev1.PodSpec{
						ServiceAccountName: goc.gw.Spec.ServiceAccountName,
						Containers: []corev1.Container{
							{
								Name:            "gateway-processor",
								ImagePullPolicy: goc.gw.Spec.ImagePullPolicy,
								Image:           goc.gw.Spec.Image,
								Env: []corev1.EnvVar{
									{
										Name:  common.GatewayTransformerPortEnvVar,
										Value: fmt.Sprintf("%d", common.GatewayTransformerPort),
									},
									{
										Name:  common.EnvVarNamespace,
										Value: goc.gw.Namespace,
									},
								},
							},
							{
								Name:            "gateway-transformer",
								ImagePullPolicy: corev1.PullAlways,
								Image:           common.GatewayEventTransformerImage,
								Env: []corev1.EnvVar{
									{
										Name:  common.GatewayTransformerConfigMapEnvVar,
										Value: common.DefaultGatewayTransformerConfigMapName(goc.gw.Name),
									},
									{
										Name:  common.EnvVarNamespace,
										Value: goc.gw.Namespace,
									},
								},
							},
						},
					},
				},
			},
		}

		// we can now create the gateway deployment.
		// depending on user configuration gateway will be exposed outside the cluster or intra-cluster.
		_, err = goc.controller.kubeClientset.AppsV1().Deployments(goc.gw.Namespace).Create(gatewayDeployment)
		if err != nil {
			goc.log.Error().Err(err).Msg("failed gateway deployment")
			goc.gw.Status = v1alpha1.NodePhaseError
		} else {
			goc.gw.Status = v1alpha1.NodePhaseRunning
			if goc.gw.Spec.Service.Port != 0 {
				goc.createGatewayService()
			}
		}

		// update state of the gateway
		goc.gw, err = gatewayClient.Update(goc.gw)
		if err != nil {
			err = goc.reapplyUpdate(gatewayClient)
			if err != nil {
				goc.log.Error().Msg("failed to update gateway")
				return err
			}
		}

		// Gateway is in error
	case v1alpha1.NodePhaseError:
		gDeployment, err := goc.controller.kubeClientset.AppsV1().Deployments(goc.gw.Namespace).Get(goc.gw.Name, metav1.GetOptions{})
		if err != nil {
			goc.log.Error().Err(err).Msg("error occurred retrieving gateway deployment")
			return err
		}

		// If image has been updated
		gDeployment.Spec.Template.Spec.Containers[0].Image = goc.gw.Spec.Image
		_, err = goc.controller.kubeClientset.AppsV1().Deployments(goc.gw.Namespace).Update(gDeployment)
		if err != nil {
			goc.log.Error().Err(err).Msg("error occurred updating gateway deployment")
			return err
		}

		// Update node phase to running
		goc.gw.Status = v1alpha1.NodePhaseRunning
		// update state of the gateway
		goc.gw, err = gatewayClient.Update(goc.gw)
		if err != nil {
			err = goc.reapplyUpdate(gatewayClient)
			if err != nil {
				goc.log.Error().Err(err).Msg("failed to update gateway")
				return err
			}
		}
		return nil

		// Gateway is already running, do nothing
	case v1alpha1.NodePhaseRunning:
		// Todo: if the sensor to which event should be dispatched changes then update the configmap for gateway pod
		goc.log.Warn().Msg("gateway is already running")
	default:
		goc.log.Panic().Str("phase", string(goc.gw.Status)).Msg("unknown gateway phase.")
	}
	return nil
}

// Creates a service that exposes gateway outside the cluster
func (goc *gwOperationCtx) createGatewayService() {
	gatewayService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      common.DefaultGatewayServiceName(goc.gw.Name),
			Namespace: goc.gw.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(goc.gw, v1alpha1.SchemaGroupVersionKind),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				common.LabelGatewayName: goc.gw.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       goc.gw.Spec.Service.Port,
					TargetPort: intstr.FromInt(goc.gw.Spec.Service.TargetPort),
				},
			},
			Type: corev1.ServiceType(goc.gw.Spec.Service.Type),
		},
	}

	_, err := goc.controller.kubeClientset.CoreV1().Services(goc.gw.Namespace).Create(gatewayService)
	// Fail silently
	if err != nil {
		goc.log.Error().Err(err).Msg("failed to create service for gateway deployment")
	}
}

// reapply the updates to gateway resource
func (goc *gwOperationCtx) reapplyUpdate(gatewayClient client.GatewayInterface) error {
	return wait.ExponentialBackoff(common.DefaultRetry, func() (bool, error) {
		g, err := gatewayClient.Get(goc.gw.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		g.Status = goc.gw.Status
		goc.gw, err = gatewayClient.Update(g)
		if err != nil {
			if !common.IsRetryableKubeAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
}
