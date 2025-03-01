package open

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/glasskube/glasskube/api/v1alpha1"
	"github.com/glasskube/glasskube/pkg/client"
	"github.com/glasskube/glasskube/pkg/future"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

type opener struct {
	pkgClient  *client.PackageV1Alpha1Client
	ksClient   kubernetes.Interface
	restClient rest.Interface
	stopCh     []chan struct{}
	readyCh    []chan struct{}
	stopped    bool
}

func NewOpener() *opener {
	return &opener{}
}

func (o *opener) Open(ctx context.Context, packageName string, entrypointName string) (*OpenResult, error) {
	if err := o.initFromContext(ctx); err != nil {
		return nil, err
	}

	var pkg v1alpha1.Package
	if err := o.pkgClient.Packages().Get(ctx, packageName, &pkg); err != nil {
		return nil, fmt.Errorf("could not get package %v: %w", packageName, err)
	}

	var packageInfo v1alpha1.PackageInfo
	if err := o.pkgClient.PackageInfos().Get(ctx, pkg.Spec.PackageInfo.Name, &packageInfo); err != nil {
		return nil, fmt.Errorf("could not get PackageInfo for package %v: %w", packageName, err)
	}

	entrypoints := packageInfo.Status.Manifest.Entrypoints
	if len(entrypoints) < 1 {
		return nil, fmt.Errorf("package has no entrypoint")
	}

	if entrypointName != "" {
		exists := false
		for _, entrypoint := range entrypoints {
			if entrypoint.Name == entrypointName {
				exists = true
				break
			}
		}
		if !exists {
			return nil, fmt.Errorf("package has no entrypoint %v", entrypointName)
		}
	}

	result := OpenResult{opener: o}
	var futures []future.Future
	for _, entrypoint := range entrypoints {
		if entrypointName == "" || entrypoint.Name == entrypointName {
			e := entrypoint
			readyCh := make(chan struct{})
			stopCh := make(chan struct{})
			o.readyCh = append(o.readyCh, readyCh)
			o.stopCh = append(o.stopCh, stopCh)
			entrypointFuture, err := o.open(ctx, packageInfo.Status.Manifest, e, readyCh, stopCh)
			if err != nil {
				o.stop()
				epName := e.Name
				if epName == "" {
					epName = "[anonymous]"
				}
				return nil, fmt.Errorf("could not open entrypoint %v: %w", epName, err)
			}
			futures = append(futures, entrypointFuture)
			// attach the first url to the result
			// TODO: Maybe there is a more elegant way to do this.
			if result.Url == "" {
				result.Url = getBrowserUrl(e)
			}
		}
	}
	result.Completion = future.All(futures...)
	return &result, nil
}

func (o *opener) initFromContext(ctx context.Context) error {
	if o.pkgClient == nil {
		o.pkgClient = client.FromContext(ctx)
	}

	if o.ksClient == nil {
		ksClient, err := kubernetes.NewForConfig(client.ConfigFromContext(ctx))
		if err != nil {
			return err
		}
		o.ksClient = ksClient
	}

	if o.restClient == nil {
		restConfig := *client.ConfigFromContext(ctx)
		restConfig.GroupVersion = &corev1.SchemeGroupVersion
		restConfig.APIPath = "/api"
		restConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
		restClient, err := rest.RESTClientFor(&restConfig)
		if err != nil {
			return err
		}
		o.restClient = restClient
	}
	return nil
}

func (o *opener) stop() {
	if !o.stopped {
		o.stopped = true
		for _, c := range o.stopCh {
			close(c)
		}
	}
}

func (o *opener) open(
	ctx context.Context,
	manifest *v1alpha1.PackageManifest,
	entrypoint v1alpha1.PackageEntrypoint,
	readyChannel chan struct{},
	stopChannel chan struct{},
) (future.Future, error) {
	if err := checkLocalPort(entrypoint); err != nil {
		return nil, err
	}

	svc, err := o.service(ctx, manifest, entrypoint)
	if err != nil {
		return nil, err
	}
	pod, err := o.pod(ctx, svc)
	if err != nil {
		return nil, err
	}
	port, err := portMapping(svc, pod, entrypoint)
	if err != nil {
		return nil, err
	}

	roundTripper, upgrader, err := spdy.RoundTripperFor(client.ConfigFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("could not create RoundTripper: %w", err)
	}

	url := o.restClient.Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward").URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, "POST", url)
	stdout := prefixWriter{prefix: fmt.Sprintf("%v\t |I| ", entrypoint.Name), writer: os.Stderr}
	stderr := prefixWriter{prefix: fmt.Sprintf("%v\t |E| ", entrypoint.Name), writer: os.Stderr}
	forwarder, err := portforward.New(dialer, []string{port}, stopChannel, readyChannel, stdout, stderr)
	if err != nil {
		return nil, fmt.Errorf("could not create PortForwarder: %w", err)
	}

	return future.Run(func() error {
		if err = forwarder.ForwardPorts(); err != nil {
			return fmt.Errorf("could not forward port %v: %w", port, err)
		} else {
			return nil
		}
	}), nil
}

func (o *opener) service(
	ctx context.Context,
	manifest *v1alpha1.PackageManifest,
	entrypoint v1alpha1.PackageEntrypoint,
) (*corev1.Service, error) {
	svc, err := o.ksClient.CoreV1().
		Services(manifest.DefaultNamespace).
		Get(ctx, entrypoint.ServiceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not get service %v: %w", entrypoint.ServiceName, err)
	} else {
		return svc, nil
	}
}

func (o *opener) pod(ctx context.Context, service *corev1.Service) (*corev1.Pod, error) {
	selector := labels.SelectorFromSet(service.Spec.Selector)
	pods, err := o.ksClient.CoreV1().
		Pods(service.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("could not get pod for service %v: %w", service.Name, err)
	}
	if len(pods.Items) < 1 {
		return nil, fmt.Errorf("no pod found for service %v", service.Name)
	}
	return &pods.Items[0], nil
}

func portMapping(service *corev1.Service, pod *corev1.Pod, entrypoint v1alpha1.PackageEntrypoint) (string, error) {
	if sp, err := servicePort(service, entrypoint); err != nil {
		return "", err
	} else if cp, err := containerPort(pod, *sp); err != nil {
		return "", err
	} else {
		return fmt.Sprintf("%v:%v", getLocalPort(entrypoint), cp), nil
	}
}

func servicePort(service *corev1.Service, entrypoint v1alpha1.PackageEntrypoint) (*corev1.ServicePort, error) {
	for _, port := range service.Spec.Ports {
		if port.Port == entrypoint.Port {
			return &port, nil
		}
	}

	return nil, fmt.Errorf("service %v has no port %v", service.Name, entrypoint.Port)
}

func containerPort(pod *corev1.Pod, servicePort corev1.ServicePort) (int32, error) {
	// A service can refer to a container port either by name or by port number. Both cases need to be covered here.
	if servicePort.TargetPort.Type == intstr.Int {
		return servicePort.TargetPort.IntVal, nil
	} else {
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.Name == servicePort.TargetPort.StrVal {
					return port.ContainerPort, nil
				}
			}
		}
		return 0, fmt.Errorf("chould not find container port for pod %v", pod.Name)
	}
}

func checkLocalPort(entrypoint v1alpha1.PackageEntrypoint) error {
	port := getLocalPort(entrypoint)
	if l, err := net.Listen("tcp", fmt.Sprintf(":%v", port)); err != nil {
		return fmt.Errorf("tcp port %v is not free", port)
	} else if err = l.Close(); err != nil {
		return fmt.Errorf("could not close listener during check: %w", err)
	} else {
		return nil
	}
}

func getLocalPort(entrypoint v1alpha1.PackageEntrypoint) int32 {
	if entrypoint.LocalPort != 0 {
		return entrypoint.LocalPort
	} else {
		return entrypoint.Port
	}
}

func getBrowserUrl(entrypoint v1alpha1.PackageEntrypoint) string {
	url := url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("localhost:%v", getLocalPort(entrypoint)),
	}
	if entrypoint.Scheme != "" {
		url.Scheme = entrypoint.Scheme
	}
	return url.String()
}
