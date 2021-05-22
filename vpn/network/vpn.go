package network

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	clientgowatch "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/exec"
	"log"
	"net/http"
	"nocalhost/internal/nhctl/syncthing/ports"
	"nocalhost/test/nhctlcli"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Sshuttle interface {
	Outbound()
	Inbound()
}
/**
 * 1) create dns server
 * 2) port-forward dns server
 * 3) generate ssh key
 * 4) sshuttle
 */
var config *restclient.Config
var clientset *kubernetes.Clientset

const namespace = "default"
const name = "dnsserver"

// the reason why using ref-count are all start operation will using single one dns pod, save resource
const refCountKey = "ref-count"

var kubeconfig = flag.String("kubeconfig", "", "your k8s cluster kubeconfig path")
var serviceName = flag.String("name", "", "service name and deployment name, should be same")
var serviceNamespace = flag.String("namespace", "", "service namespace")
var portPair = flag.String("expose", "", "port pair, remote-port:local-port, such as: service-port1:local-port1,service-port2:local-port2...")

func Start() {
	flag.Parse()
	log.Printf("kubeconfig path: %s\n", *kubeconfig)
	err := preCheck()
	if err != nil {
		panic(err)
	}
	privateKeyPath := filepath.Join(HomeDir(), ".nh", "ssh", "private", "key")
	public := filepath.Join(HomeDir(), ".nh", "ssh", "private", "pub")
	sshInfo := generateSSH(privateKeyPath, public)
	initClient(*kubeconfig)
	addCleanUpResource()
	serviceIp, _ := createDnsPod(sshInfo)
	// random port
	localSshPort, err := ports.GetAvailablePort()
	if err != nil {
		log.Fatal(err)
	}
	go portForwardService(*kubeconfig, localSshPort)
	time.Sleep(3 * time.Second)
	updateRefCount(1)
	log.Println("port forward ready")
	// todo cidr network
	if runtime.GOOS == "windows" || os.Getenv("debug") != "" {
		go sshD(privateKeyPath, localSshPort)
	} else {
		go sshuttle(serviceIp, privateKeyPath, localSshPort)
	}
	time.Sleep(6 * time.Second)
	Inbound(privateKeyPath)
	select {}
}

/**
 * 1) scale deployment replicas to zero
 * 2) relabel new shadow pod, make sure traffic which from service will receive by shadow pod
 * 3) using another images to listen local and transfer traffic
 */
func Inbound(privateKeyPath string) {
	if serviceName == nil || serviceNamespace == nil || portPair == nil ||
		*serviceName == "" || *serviceNamespace == "" || *portPair == "" {
		log.Println("no need to expose local service to remote")
		return
	}
	log.Println("prepare to expose local service to remote")

	podShadow := createPodShadow()
	if podShadow == nil {
		log.Println("fail to create shadow")
		return
	}
	log.Printf("wait for shadow: %s to be ready...\n", podShadow.Name)
	WaitPodToBeStatus(*serviceNamespace, "name="+podShadow.Name, func(pod *v1.Pod) bool { return v1.PodRunning == pod.Status.Phase })

	localSshPort, err := ports.GetAvailablePort()
	if err != nil {
		log.Fatal(err)
	}
	readyChan := make(chan struct{})
	stopsChan := make(chan struct{})
	go func() {
		if err = portForwardPod(podShadow.Name, podShadow.Namespace, localSshPort, readyChan, stopsChan); err != nil {
			log.Printf("port forward error, exiting")
			panic(err)
		}
	}()
	<-readyChan
	log.Println("port forward ready")
	remote2Local := strings.Split(*portPair, ",")
	for _, pair := range remote2Local {
		p := strings.Split(pair, ":")
		go sshReverseProxy(p[0], p[1], localSshPort, privateKeyPath)
	}
	// hang up
	select {}
}

// multiple remote service port
func sshReverseProxy(remotePort, localPort string, sshLocalPort int, privateKeyPath string) {
	log.Println("prepare to start reverse proxy")
	cmd := osexec.Command("ssh", "-NR",
		fmt.Sprintf("0.0.0.0:%s:127.0.0.1:%s", remotePort, localPort),
		"root@127.0.0.1", "-p", strconv.Itoa(sshLocalPort),
		"-oStrictHostKeyChecking=no",
		"-oUserKnownHostsFile=/dev/null",
		"-i",
		privateKeyPath)
	stdout, stderr, err := nhctlcli.Runner.RunWithRollingOut(cmd)
	if err != nil {
		log.Printf("start reverse proxy failed, error: %v, stdout: %s, stderr: %s", err, stdout, stderr)
		stopChan <- syscall.SIGQUIT
		return
	}
}

func createPodShadow() *v1.Pod {
	service, err := clientset.CoreV1().Services(*serviceNamespace).Get(context.TODO(), *serviceName, metav1.GetOptions{})
	if err != nil {
		log.Println(err)
	}
	labels := service.Spec.Selector
	// todo version
	newName := *serviceName + "-" + "shadow"
	deployment, err := clientset.AppsV1().Deployments(*serviceNamespace).Get(context.TODO(), *serviceName, metav1.GetOptions{})
	if err != nil {
		log.Println(err)
		return nil
	}
	ForkSshConfigMapToNamespace()
	pod := newPod(newName, *serviceNamespace, labels, deployment.Spec.Template.Spec.Containers[0].Ports)
	_ = clientset.CoreV1().Pods(*serviceNamespace).Delete(context.TODO(), newName, metav1.DeleteOptions{})
	pods, err := clientset.CoreV1().Pods(*serviceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		log.Println(err)
	}
	return pods
}

func ForkSshConfigMapToNamespace() {
	_, err := clientset.CoreV1().ConfigMaps(*serviceNamespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		log.Printf("ssh configmap already exist in namespace: %s, no need to fork it\n", *serviceNamespace)
		return
	}
	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		log.Printf("can't find configmap: %s in namespace: %s\n", name, namespace)
		return
	}
	newConfigmap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: *serviceNamespace,
		},
		Data: configMap.Data,
	}
	_, err = clientset.CoreV1().ConfigMaps(*serviceNamespace).Create(context.TODO(), &newConfigmap, metav1.CreateOptions{})
	if err == nil {
		log.Printf("fork configmap secuessfully")
	} else {
		log.Printf("fork configmap failed")
	}
}

func scaleDeploymentReplicasToZero() {
	_, err := clientset.AppsV1().Deployments(*serviceNamespace).
		UpdateScale(context.TODO(), *serviceName, &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: 0}}, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("update deployment: %s to replicas to zero failed, error: %v\n", *serviceName, err)
	}
}

var stopChan = make(chan os.Signal)

func addCleanUpResource() {

	signal.Notify(stopChan, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	go func() {
		<-stopChan
		log.Println("prepare to exit, cleaning up")
		cleanUp()
		cleanHosts()
		log.Println("clean up successful")
		os.Exit(0)
	}()
}

// vendor/k8s.io/kubectl/pkg/polymorphichelpers/rollback.go:99
func updateRefCount(increment int) {
	get, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		log.Println("this should not happened")
		return
	}
	curCount, err := strconv.Atoi(get.GetAnnotations()[refCountKey])
	if err != nil {
		curCount = 0
	}

	patch, _ := json.Marshal([]interface{}{
		map[string]interface{}{
			"op":    "replace",
			"path":  "/metadata/annotations/" + refCountKey,
			"value": strconv.Itoa(curCount + increment),
		},
	})
	_, err = clientset.CoreV1().ConfigMaps(namespace).Patch(context.TODO(),
		name, types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		log.Printf("update ref count error, error: %v\n", err)
	} else {
		log.Println("update ref count successfully")
	}
}

func preCheck() error {
	_, err := osexec.LookPath("kubectl")
	if err != nil {
		log.Println("can not found kubectl, please install it previously")
		return err
	}
	_, err = osexec.LookPath("sshuttle")
	if err != nil {
		if _, err = osexec.LookPath("brew"); err == nil {
			log.Println("try to install sshuttle")
			cmd := osexec.Command("brew", "install", "sshuttle")
			_, stderr, err2 := nhctlcli.Runner.RunWithRollingOut(cmd)
			if err2 != nil {
				log.Printf("try to install sshuttle failed, error: %v, stderr: %s", err2, stderr)
				return err2
			}
			fmt.Println("install sshuttle successfully")
		}
	}
	return nil
}

func cleanUp() {
	updateRefCount(-1)
	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		log.Println(err)
		return
	}
	refCount, err := strconv.Atoi(configMap.GetAnnotations()[refCountKey])
	if err != nil {
		log.Println(err)
	}
	// if refcount is less than zero or equals to zero, means no body will using this dns pod, so clean it
	if refCount <= 0 {
		log.Println("refCount is zero, prepare to clean up resource")
		_ = clientset.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
		_ = clientset.AppsV1().Deployments(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
		_ = clientset.CoreV1().Services(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	}
}

func cleanHosts() {
	if _, err := os.Stat("/etc/hosts.bak"); err != nil {
		log.Println("no backup host file found, no needs to restore")
	}
	_, err2 := CopyFile("/etc/hosts", "/etc/hosts.bak")
	if err2 != nil {
		log.Printf("restore hosts file failed")
	} else {
		log.Printf("restore hosts file secuessfully")
	}
	_ = os.Remove("/etc/hosts.bak")
}

func initClient(kubeconfigPath string) {
	if _, err := os.Stat(kubeconfigPath); err != nil {
		kubeconfigPath = filepath.Join(HomeDir(), ".kube", "config")
	}
	config, _ = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	clientset, _ = kubernetes.NewForConfig(config)
}

func createDnsPod(info *SSHInfo) (serviceIp, podName string) {
	configmap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			log.Println("prepare to create configmap")
			_, err = clientset.CoreV1().
				ConfigMaps(namespace).
				Create(context.Background(), newSshConfigmap(info.PrivateKeyBytes, info.PublicKeyBytes), metav1.CreateOptions{})
			if err != nil {
				log.Println(err)
			}
		} else {
			log.Fatal(err)
		}
	} else {
		log.Println("configmap already exist, dump private key")
		s := configmap.Data["privateKey"]
		err = ioutil.WriteFile(filepath.Join(HomeDir(), ".nh", "ssh", "private", "key"), []byte(s), 0700)
		if err != nil {
			log.Println(err)
		}
		log.Println("dump private key ok")
	}

	_, err = clientset.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			log.Println("prepare to create deployment: " + name)
			_, err = clientset.AppsV1().Deployments(namespace).Create(context.Background(), newDnsPodDeployment(), metav1.CreateOptions{})
			if err != nil {
				log.Println(err)
			}
			log.Println("create deployment successfully")
		} else {
			log.Fatal(err)
		}
	} else {
		log.Println("deployment already exist")
	}
	log.Println("wait for pod ready...")
	WaitPodToBeStatus(namespace, "app="+name, func(pod *v1.Pod) bool { return v1.PodRunning == pod.Status.Phase })
	list, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=" + name})
	if err != nil {
		log.Fatal(err)
	}
	podName = list.Items[0].Name
	log.Println("pod ready")
	service, err := clientset.CoreV1().Services(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			log.Println("prepare to create service: " + name)
			service, err = clientset.CoreV1().Services(namespace).Create(context.Background(), newDnsPodService(), metav1.CreateOptions{})
		}
	} else {
		log.Println("service already exist")
		log.Println("service ip: " + service.Spec.ClusterIP)
	}
	serviceIp = service.Spec.ClusterIP
	return
}

func portForwardPod(podName string, namespace string, port int, readyChan, stopChan chan struct{}) error {
	log.Printf("%s-%s\n", "pods", podName)
	url := clientset.CoreV1().
		RESTClient().
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	log.Println("url: " + url.String())
	if err != nil {
		log.Println(err)
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	out := new(bytes.Buffer)
	p := []string{fmt.Sprintf("%d:%d", port, 22)}
	forwarder, err := portforward.New(dialer, p, stopChan, readyChan, ioutil.Discard, out)
	if err != nil {
		log.Println(err)
		return err
	}

	if err = forwarder.ForwardPorts(); err != nil {
		panic(err)
	}
	return nil
}

func WaitPodToBeStatus(namespace string, label string, checker func(*v1.Pod) bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	watchlist := cache.NewFilteredListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		namespace,
		func(options *metav1.ListOptions) { options.LabelSelector = label })

	preConditionFunc := func(store cache.Store) (bool, error) {
		if len(store.List()) == 0 {
			return false, nil
		}
		for _, p := range store.List() {
			if !checker(p.(*v1.Pod)) {
				return false, nil
			}
		}
		return true, nil
	}
	conditionFunc := func(e watch.Event) (bool, error) { return checker(e.Object.(*v1.Pod)), nil }
	event, err := clientgowatch.UntilWithSync(ctx, watchlist, &v1.Pod{}, preConditionFunc, conditionFunc)
	if err != nil {
		log.Printf("wait pod has the label: %s to ready failed, error: %v, event: %v", label, err, event)
		return false
	}
	return true
}

func portForwardService(kubeconfigPath string, localSshPort int) {
	cmd := exec.New().
		CommandContext(
			context.TODO(),
			"kubectl",
			"port-forward",
			"service/dnsserver",
			strconv.Itoa(localSshPort)+":22",
			"--namespace",
			"default",
			"--kubeconfig", kubeconfigPath)
	cmd.Start()
	err := cmd.Wait()
	if err != nil {
		log.Println(err)
	}
}

func sshuttle(serviceIp, sshPrivateKeyPath string, localSshPort int) {
	args := []string{
		"-r",
		"root@127.0.0.1:" + strconv.Itoa(localSshPort),
		"-e", "ssh -oStrictHostKeyChecking=no -oUserKnownHostsFile=/dev/null -i " + sshPrivateKeyPath,
		"0/0",
		"-x",
		"127.0.0.1",
		"--dns",
		"--to-ns",
		serviceIp,
	}
	cmd := osexec.CommandContext(context.TODO(), "sshuttle", args...)
	out, s, err := nhctlcli.Runner.RunWithRollingOut(cmd)
	if err != nil {
		log.Printf("error: %v, stdout: %s, stderr: %s", err, out, s)
	} else {
		log.Printf(out)
	}
}

func newSshConfigmap(privateKey, publicKey []byte) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{refCountKey: "0"},
		},
		Data: map[string]string{"authorized": string(publicKey), "privateKey": string(privateKey)},
	}
}

func newDnsPodDeployment() *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "apps",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
					Labels:    map[string]string{"app": name},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:  name,
						Image: "naison/dnsserver:latest",
						Ports: []v1.ContainerPort{
							{ContainerPort: 53, Protocol: v1.ProtocolTCP},
							{ContainerPort: 53, Protocol: v1.ProtocolUDP},
							{ContainerPort: 22},
						},
						ImagePullPolicy: v1.PullAlways,
						VolumeMounts: []v1.VolumeMount{{
							Name:      "ssh-key",
							MountPath: "/root",
						}},
					}},
					Volumes: []v1.Volume{{
						Name: "ssh-key",
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: name,
								},
								Items: []v1.KeyToPath{{
									Key:  "authorized",
									Path: "authorized_keys",
								}},
							},
						},
					}},
				},
			},
		},
	}
}

func newDnsPodService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{Name: "tcp", Protocol: v1.ProtocolTCP, Port: 53, TargetPort: intstr.FromInt(53)},
				{Name: "udp", Protocol: v1.ProtocolUDP, Port: 53, TargetPort: intstr.FromInt(53)},
				{Name: "ssh", Port: 22, TargetPort: intstr.FromInt(22)},
			},
			Selector: map[string]string{"app": name},
			Type:     v1.ServiceTypeClusterIP,
		},
	}
}

func newPod(podName, namespace string, labels map[string]string, port []v1.ContainerPort) *v1.Pod {
	labels["nocalhost"] = "nocalhost"
	labels["name"] = podName
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Image:           "naison/dnsserver:latest",
				Ports:           port,
				Name:            podName,
				ImagePullPolicy: v1.PullAlways,
				VolumeMounts: []v1.VolumeMount{{
					Name:      "ssh-key",
					MountPath: "/root",
				}},
			}},
			Volumes: []v1.Volume{{
				Name: "ssh-key",
				VolumeSource: v1.VolumeSource{
					ConfigMap: &v1.ConfigMapVolumeSource{
						LocalObjectReference: v1.LocalObjectReference{
							Name: name,
						},
						Items: []v1.KeyToPath{{
							Key:  "authorized",
							Path: "authorized_keys",
						}},
					},
				},
			}},
		},
	}
}