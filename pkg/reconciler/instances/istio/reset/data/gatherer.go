package data

import (
	"context"
	"encoding/json"

	"github.com/avast/retry-go"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio/helpers"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

//go:generate mockery --name=Gatherer --outpkg=mocks --case=underscore
// Gatherer gathers data from the Kubernetes cluster.
type Gatherer interface {
	// GetAllPods from the cluster and return them as a v1.PodList.
	GetAllPods(kubeClient kubernetes.Interface, retryOpts []retry.Option) (podsList *v1.PodList, err error)

	// GetPodsWithDifferentImage than the passed expected image to filter them out from the pods list.
	GetPodsWithDifferentImage(inputPodsList v1.PodList, image ExpectedImage) (outputPodsList v1.PodList, err error)

	// GetPodsWithoutSidecar return a list of pods which should have a sidecar injected but do not have it.
	GetPodsWithoutSidecar(kubeClient kubernetes.Interface, retryOpts []retry.Option) (podsList v1.PodList, err error)
}

// DefaultGatherer that gets pods from the Kubernetes cluster
type DefaultGatherer struct{}

// ExpectedImage to be verified by the proxy.
type ExpectedImage struct {
	Version helpers.HelperVersion
}

// NewDefaultGatherer creates a new instance of DefaultGatherer.
func NewDefaultGatherer() *DefaultGatherer {
	return &DefaultGatherer{}
}

func (i *DefaultGatherer) GetAllPods(kubeClient kubernetes.Interface, retryOpts []retry.Option) (podsList *v1.PodList, err error) {
	err = retry.Do(func() error {
		podsList, err = kubeClient.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return err
		}

		return nil
	}, retryOpts...)

	if err != nil {
		return nil, err
	}

	return
}

func (i *DefaultGatherer) GetPodsWithDifferentImage(inputPodsList v1.PodList, image ExpectedImage) (outputPodsList v1.PodList, err error) {
	inputPodsList.DeepCopyInto(&outputPodsList)
	outputPodsList.Items = []v1.Pod{}

	for _, pod := range inputPodsList.Items {
		if _, containsIstioSidecarAnnotation := pod.Annotations["sidecar.istio.io/status"]; !containsIstioSidecarAnnotation || !isPodReady(pod) {
			continue
		}

		istioSidecarNames := getIstioSidecarNamesFromAnnotations(pod.Annotations)

		for _, container := range pod.Spec.Containers {
			if !isIstioSidecar(istioSidecarNames, container.Name) {
				continue
			}
			containerVersion, err := helpers.NewHelperVersionFrom(container.Image)
			if err != nil {
				return v1.PodList{}, err
			}

			sameLibrary := containerVersion.Library == image.Version.Library
			sameTag := containerVersion.Tag.Equal(image.Version.Tag)
			if !sameTag || !sameLibrary {
				outputPodsList.Items = append(outputPodsList.Items, *pod.DeepCopy())
			}
		}
	}

	return outputPodsList, nil
}

func (i *DefaultGatherer) GetPodsWithoutSidecar(kubeClient kubernetes.Interface, retryOpts []retry.Option) (podsList v1.PodList, err error) {
	allPodsWithNamespaceAnnotations, err := getAllPodsWithNamespaceAnnotations(kubeClient, retryOpts)
	if err != nil {
		return
	}

	// filter pods
	podsList = getPodsWithAnnotation(allPodsWithNamespaceAnnotations)
	podsList = getPodsWithoutSidecar(podsList)
	return
}

func getPodsWithAnnotation(inputPodsList v1.PodList) (outputPodsList v1.PodList) {
	inputPodsList.DeepCopyInto(&outputPodsList)
	outputPodsList.Items = []v1.Pod{}

	for _, pod := range inputPodsList.Items {
		if pod.Annotations["sidecar.istio.io/inject"] == "false" || pod.Annotations["reconciler/namespace-istio-injection"] == "disabled" {
			continue
		} else {
			outputPodsList.Items = append(outputPodsList.Items, *pod.DeepCopy())
		}
	}

	return
}

func getPodsWithoutSidecar(inputPodsList v1.PodList) (outputPodsList v1.PodList) {
	inputPodsList.DeepCopyInto(&outputPodsList)
	outputPodsList.Items = []v1.Pod{}

	for _, pod := range inputPodsList.Items {
		if !isPodReady(pod) {
			continue
		}
		//Automatic sidecar injection is ignored for pods on the host network
		if pod.Spec.HostNetwork {
			continue
		}

		if !hasIstioProxy(pod.Spec.Containers, "istio-proxy") {
			outputPodsList.Items = append(outputPodsList.Items, *pod.DeepCopy())
		}

	}

	return
}

func hasIstioProxy(containers []v1.Container, proxyName string) bool {
	proxyImage := ""
	for _, container := range containers {
		if container.Name == proxyName {
			proxyImage = container.Image
		}
	}
	return proxyImage != ""
}

func getAllPodsWithNamespaceAnnotations(kubeClient kubernetes.Interface, retryOpts []retry.Option) (podsList v1.PodList, err error) {
	var namespaces *v1.NamespaceList
	err = retry.Do(func() error {
		namespaces, err = kubeClient.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		return nil
	}, retryOpts...)
	if err != nil {
		return podsList, err
	}

	err = retry.Do(func() error {
		for _, namespace := range namespaces.Items {
			if namespace.ObjectMeta.Name == "kube-system" {
				continue
			}

			pods, err := kubeClient.CoreV1().Pods(namespace.Name).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return err
			}

			for _, pod := range pods.Items {
				if _, isNamespaceLabeled := namespace.Labels["istio-injection"]; isNamespaceLabeled {
					pod.Annotations["reconciler/namespace-istio-injection"] = namespace.Labels["istio-injection"]
				}
				podsList.Items = append(podsList.Items, pod)
			}
		}

		return nil
	}, retryOpts...)
	if err != nil {
		return podsList, err
	}

	return
}

// getIstioSidecarNamesFromAnnotations gets all container names in pod annoted with podAnnotations that are Istio sidecars
func getIstioSidecarNamesFromAnnotations(podAnnotations map[string]string) []string {
	type istioStatusStruct struct {
		Containers []string `json:"containers"`
	}
	istioStatus := istioStatusStruct{}
	err := json.Unmarshal([]byte(podAnnotations["sidecar.istio.io/status"]), &istioStatus)
	if err != nil {
		return []string{}
	}
	return istioStatus.Containers
}

// isIstioSidecar checks whether the container with name=containerName is the Istio sidecar in pod with Istio sidecars with names=istioSidecarNames
func isIstioSidecar(istioSidecarNames []string, containerName string) bool {
	for _, c := range istioSidecarNames {
		if c == containerName {
			return true
		}
	}
	return false
}

// isPodReady checks if the pod is Ready, returns true if the Pod is in the Running state and not Pending or Terminating.
func isPodReady(pod v1.Pod) bool {

	if pod.Status.Phase != v1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Status != v1.ConditionTrue {
			return false
		}
	}

	return pod.ObjectMeta.DeletionTimestamp == nil
}

// RemoveAnnotatedPods removes pods with annotation annotationKey from in podList
func RemoveAnnotatedPods(in v1.PodList, annotationKey string) (out v1.PodList) {
	in.DeepCopyInto(&out)
	out.Items = []v1.Pod{}
	for i := 0; i < len(in.Items); i++ {
		if _, ok := in.Items[i].Annotations[annotationKey]; !ok {
			out.Items = append(out.Items, in.Items[i])
		}
	}
	return
}
