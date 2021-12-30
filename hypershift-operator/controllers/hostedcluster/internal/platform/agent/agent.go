package agent

import (
	"context"
	"fmt"

	agentv1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/controlplaneoperator"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/support/upsert"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sutilspointer "k8s.io/utils/pointer"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	//TODO set image from config?
	imageCAPA = "quay.io/edge-infrastructure/cluster-api-provider-agent:latest-2021-12-30"
)

type Agent struct{}

func (p Agent) ReconcileCAPIInfraCR(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string, apiEndpoint hyperv1.APIEndpoint) (client.Object, error) {

	hcp := controlplaneoperator.HostedControlPlane(controlPlaneNamespace, hcluster.Name)
	if err := c.Get(ctx, client.ObjectKeyFromObject(hcp), hcp); err != nil {
		return nil, fmt.Errorf("failed to get control plane ref: %w", err)
	}

	agentCluster := &agentv1.AgentCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: controlPlaneNamespace,
			Name:      hcluster.Name,
		},
	}

	_, err := createOrUpdate(ctx, c, agentCluster, func() error {
		return reconcileAgentCluster(agentCluster, hcluster, hcp)
	})
	if err != nil {
		return nil, err
	}

	return agentCluster, nil
}

func (p Agent) CAPIProviderDeploymentSpec(hcluster *hyperv1.HostedCluster, tokenMinterImage string) (*appsv1.DeploymentSpec, error) {
	deploymentSpec := &appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"control-plane": "controller-manager",
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"control-plane": "controller-manager",
				},
			},
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: k8sutilspointer.Int64Ptr(10),
				ServiceAccountName:            "cluster-api-provider-agent-controller-manager",
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: k8sutilspointer.BoolPtr(true),
				},
				Containers: []corev1.Container{
					{
						Name:    "manager",
						Image:   imageCAPA,
						Command: []string{"/manager"},
						Args: []string{
							"--health-probe-bind-address=:8081",
							"--metrics-bind-address=127.0.0.1:8080",
							"--leader-elect",
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("8081"),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/readyz",
									Port: intstr.FromString("8081"),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("100Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("20Mi"),
							},
						},
					},
					{
						Name:  "kube-rbac-proxy",
						Image: "gcr.io/kubebuilder/kube-rbac-proxy:v0.8.0",
						Args: []string{
							"--secure-listen-address=0.0.0.0:8443",
							"--upstream=http://127.0.0.1:8080/",
							"--logtostderr=true",
							"--v=10",
						},
						Ports: []corev1.ContainerPort{
							{
								Name:          "https",
								ContainerPort: 8443,
								Protocol:      corev1.ProtocolTCP,
							},
						},
					},
				},
			},
		},
	}
	return deploymentSpec, nil
}

func (p Agent) ReconcileCredentials(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string) error {
	return nil
}

func (Agent) ReconcileSecretEncryption(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string) error {
	return nil
}

func (Agent) CAPIProviderPolicyRules() []rbacv1.PolicyRule {
	return nil
}

func reconcileAgentCluster(agentCluster *agentv1.AgentCluster, hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) error {
	agentCluster.Spec.ReleaseImage = hcp.Spec.ReleaseImage
	agentCluster.Spec.ClusterName = hcluster.Name
	agentCluster.Spec.BaseDomain = hcluster.Spec.DNS.BaseDomain
	agentCluster.Spec.PullSecretRef = &hcp.Spec.PullSecret

	caSecret := ignitionserver.IgnitionCACertSecret(hcp.Namespace)
	if hcluster.Status.IgnitionEndpoint != "" {
		agentCluster.Spec.IgnitionEndpoint = &agentv1.IgnitionEndpoint{
			Url:                    "https://" + hcluster.Status.IgnitionEndpoint + "/ignition",
			CaCertificateReference: &agentv1.CaCertificateReference{Name: caSecret.Name, Namespace: caSecret.Namespace}}
	}
	agentCluster.Spec.ControlPlaneEndpoint = capiv1.APIEndpoint{
		Host: hcp.Status.ControlPlaneEndpoint.Host,
		Port: hcp.Status.ControlPlaneEndpoint.Port,
	}

	return nil
}
