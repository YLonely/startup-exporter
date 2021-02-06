package main

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	gocontext "context"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	metricsNamespace              = "startup_exporter"
	metricsSubsystemPod           = "pod"
	metricsSubsystemDeploy        = "deployment"
	defaultContainerdK8sNamespace = "k8s.io"
	containerNamePrefix           = "containerd://"
	maxContainerNameLength        = 10
)

type meta struct {
	name      string
	namespace string
}

var (
	allInfo                     = map[meta]containerStartupInfo{}
	updatedDeploy               = map[meta]map[string]struct{}{}
	mu                          sync.Mutex
	deployPodsAvgStartupLatency = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystemPod,
			Name:      "average_startup_latency_milliseconds",
		},
		[]string{
			"deploy_name",
			"namespace",
		},
	)
	deployScaleLatency = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystemDeploy,
			Name:      "scale_latency_milliseconds",
		},
		[]string{
			"deploy_name",
			"namespace",
		},
	)
)

var exportCmd = cli.Command{
	Name:      "export",
	Usage:     "export startup metrics of containers to other service",
	ArgsUsage: "PORT",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "kubeconfig,c",
			Usage: "path to a kubeconfig",
		},
		cli.StringFlag{
			Name:  "master",
			Usage: "the address of the API server",
		},
	},
	Action: func(context *cli.Context) error {
		port := context.Args().First()
		if port == "" {
			return errors.New("port must be provided")
		}
		clientCmdConfig, err := clientcmd.BuildConfigFromFlags(context.String("master"), context.String("kubeconfig"))
		if err != nil {
			return err
		}
		signalC := make(chan os.Signal, 1024)
		signal.Notify(signalC, handledSignals...)
		done := handleSignals(signalC)
		kubeClient, err := kubernetes.NewForConfig(clientCmdConfig)
		if err != nil {
			return err
		}
		go updateDeployScaleLatency(kubeClient, done)
		svr := &http.Server{
			Addr: "0.0.0.0:" + port,
		}
		http.HandleFunc("/", receiveStartupInfo)
		http.Handle("/metrics", promhttp.Handler())
		logrus.Info("exporter started")
		exit := make(chan struct{})
		go func() {
			<-done
			svr.Shutdown(gocontext.Background())
			close(exit)
		}()
		if err = svr.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		<-exit
		logrus.Info("shutting down")
		return nil
	},
}

func receiveStartupInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var info containerStartupInfo
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&info); err != nil {
		logrus.WithError(err).Error("failed to decode data")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	m := meta{
		name:      info.Name,
		namespace: info.Namespace,
	}
	if _, exists := allInfo[m]; !exists {
		allInfo[m] = info
		logrus.WithFields(logrus.Fields{
			"name":      containerShortName(info.Name),
			"namespace": info.Namespace,
			"start":     info.Start,
			"end":       info.End,
		}).Debug("received a new container")
	}
	w.WriteHeader(http.StatusOK)
}

func updateDeployScaleLatency(kubeClient *kubernetes.Clientset, done <-chan struct{}) {
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 5*time.Second)
	deploymentLister := kubeInformerFactory.Apps().V1().Deployments().Lister()
	podLister := kubeInformerFactory.Core().V1().Pods().Lister()
	go kubeInformerFactory.Start(done)
	ticker := time.NewTicker(2 * time.Second)
	stop := false
	for {
		deployments, err := deploymentLister.List(labels.Everything())
		if err != nil {
			logrus.WithError(err).Error("failed to list deployments in the cluster")
		} else {
			for _, d := range deployments {
				if d != nil {
					m := meta{name: d.Name, namespace: d.Namespace}
					if d.Spec.Selector == nil {
						logrus.Errorf("deployment %s from %s has an empty selector", d.Name, d.Namespace)
						continue
					}
					pods, err := podLister.Pods(d.Namespace).List(makeSelector(*d.Spec.Selector))
					if err != nil {
						logrus.WithError(err).Errorf("failed to list pods belongs to %s", d.Name)
					}
					if !shouldUpdate(m, pods) {
						continue
					}
					logrus.Debugf("new deployment %s from %s", d.Name, d.Namespace)
					if updated, err := doUpdate(d, pods); err != nil {
						logrus.Error(err)
					} else if updated {
						logrus.Debugf("update deployment %s(%s) successfully", d.Name, d.Namespace)
						updatedDeploy[m] = getPodNames(pods)
					}
				}
			}
		}
		select {
		case <-done:
			stop = true
		case <-ticker.C:
		}
		if stop {
			break
		}
	}
}

func getPodNames(pods []*corev1.Pod) map[string]struct{} {
	podNames := map[string]struct{}{}
	for _, p := range pods {
		if p != nil {
			podNames[p.Name] = struct{}{}
		}
	}
	return podNames
}

func shouldUpdate(m meta, currentPods []*corev1.Pod) bool {
	if len(currentPods) == 0 {
		return false
	}
	currentPodNames := map[string]struct{}{}
	for _, p := range currentPods {
		if p != nil {
			if len(p.Status.ContainerStatuses) == 0 {
				return false
			}
			for _, c := range p.Status.ContainerStatuses {
				if strings.Trim(c.ContainerID, " ") == "" {
					// at least one container is not running
					return false
				}
			}
			currentPodNames[p.Name] = struct{}{}
		}
	}
	lastPodNames, exists := updatedDeploy[m]
	if !exists {
		return true
	}
	return !equal(lastPodNames, currentPodNames)
}

func equal(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, exists := b[k]; !exists {
			return false
		}
	}
	return true
}

func doUpdate(deploy *appsv1.Deployment, pods []*corev1.Pod) (bool, error) {
	var (
		targetLen       = 0
		total           float64
		startTimestamp  int64 = math.MaxInt64
		endTimestamp    int64 = 0
		unreceivedNames []string
		name            string
		lastPodNames    = map[string]struct{}{}
	)
	if l, exists := updatedDeploy[meta{name: deploy.Name, namespace: deploy.Namespace}]; exists {
		lastPodNames = l
	}
	for _, p := range pods {
		if p != nil {
			targetLen += len(p.Spec.Containers)
			_, oldPod := lastPodNames[p.Name]
			for _, c := range p.Status.ContainerStatuses {
				if strings.HasPrefix(c.ContainerID, containerNamePrefix) {
					name = strings.TrimPrefix(c.ContainerID, containerNamePrefix)
				} else {
					return false, errors.Errorf("container %s(%s) of deployment %s(%s) is not running by containerd", c.Name, c.ContainerID, p.Name, p.Namespace)
				}
				mu.Lock()
				if info, exists := allInfo[meta{name: name, namespace: defaultContainerdK8sNamespace}]; exists {
					if !oldPod {
						if info.Start < startTimestamp {
							startTimestamp = info.Start
						}
						if info.End > endTimestamp {
							endTimestamp = info.End
						}
					}
					total += float64(info.End - info.Start)
				} else {
					unreceivedNames = append(unreceivedNames, containerShortName(name))
				}
				mu.Unlock()
			}
		}
	}
	receivedLen := targetLen - len(unreceivedNames)
	logrus.Debugf("%d containers total, %d received, need %v", targetLen, receivedLen, unreceivedNames)
	if receivedLen == 0 {
		return false, nil
	}
	if receivedLen != targetLen {
		return false, nil
	}
	avg := total / float64(receivedLen)
	logrus.Debugf("update average startup latency of deployment %s(%s) to %v", deploy.Name, deploy.Namespace, avg)
	deployPodsAvgStartupLatency.WithLabelValues(deploy.Name, deploy.Namespace).Set(avg)
	if endTimestamp > startTimestamp {
		logrus.Debugf("update scale latency of deployment %s(%s) to %d", deploy.Name, deploy.Namespace, endTimestamp-startTimestamp)
		deployScaleLatency.WithLabelValues(deploy.Name, deploy.Namespace).Set(float64(endTimestamp - startTimestamp))
	}
	return true, nil
}

func makeSelector(labelSeletor metav1.LabelSelector) labels.Selector {
	selector := labels.NewSelector()
	for k, v := range labelSeletor.MatchLabels {
		rr, err := labels.NewRequirement(k, selection.Equals, []string{v})
		if err != nil {
			panic(err)
		}
		selector = selector.Add(*rr)
	}
	return selector
}

func containerShortName(name string) string {
	n := len(name)
	if n > maxContainerNameLength {
		return name[:maxContainerNameLength]
	}
	return name
}
