package autoscaler

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	corev1lister "k8s.io/client-go/listers/core/v1"
)

type proxyScrapeClient struct {
	httpClient         *http.Client
	metric             *Metric
	podLister          corev1lister.PodLister
	lastScrapedRequest map[string]int64
	proxyType          string
}

func newProxyScrapeClient(httpClient *http.Client, metric *Metric, podLister corev1lister.PodLister, proxyType string) (*proxyScrapeClient, error) {
	if httpClient == nil {
		return nil, errors.New("HTTP client must not be nil")
	}

	return &proxyScrapeClient{
		httpClient:         httpClient,
		metric:             metric,
		podLister:          podLister,
		lastScrapedRequest: map[string]int64{},
		proxyType:          proxyType,
	}, nil
}

func (c *proxyScrapeClient) Scrape(url string) (*Stat, error) {
	r1, err := labels.NewRequirement("app", selection.Equals, []string{c.metric.Labels["app"]})
	if err != nil {
		return nil, err
	}
	r2, err := labels.NewRequirement("version", selection.Equals, []string{c.metric.Labels["version"]})
	if err != nil {
		return nil, err
	}
	selector := labels.NewSelector().Add(*r1, *r2)
	pods, err := c.podLister.Pods(c.metric.Namespace).List(selector)
	if err != nil {
		return nil, err
	}

	var activeRequest, totalRequest, podCount int64
	now := time.Now()
	metricKey := fmt.Sprintf("%s/%s", c.metric.Namespace, c.metric.Name)
	stats := &Stat{
		Time:    &now,
		PodName: scraperPodName,
	}
	pod := &corev1.Pod{}
	podMap := map[string]*corev1.Pod{}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning {
			podMap[p.Name] = p
		}
	}
	podCount = int64(len(podMap))
	for _, p := range podMap {
		pod = p
	}
	if pod == nil {
	    return stats, nil
	}

	var metricURL, rqMatchCriteria, acrqMatchCriteria string
	if c.proxyType == "envoy" {
		metricURL = fmt.Sprintf("http://%s:15090/stats/prometheus", pod.Status.PodIP)
		port := c.metric.Labels["container-port"]
		if port == "" {
			for _, con := range pod.Spec.Containers {
				if con.Name == c.metric.Name {
					if len(con.Ports) > 0 {
						port = strconv.Itoa(int(con.Ports[0].ContainerPort))
					}
				}
			}
		}
		if port == "" {
			port = "80"
		}

		rqMatchCriteria = fmt.Sprintf("envoy_http_downstream_rq_total{http_conn_manager_prefix=\"%s_%s\"}", pod.Status.PodIP, port)
		acrqMatchCriteria = fmt.Sprintf("envoy_http_downstream_rq_active{http_conn_manager_prefix=\"%s_%s\"}", pod.Status.PodIP, port)
	} else if c.proxyType == "linkerd" {
		metricURL = fmt.Sprintf("http://%s:4191/metrics", pod.Status.PodIP)
		rqMatchCriteria = "request_handle_us_count{direction=\"inbound\"}"
		acrqMatchCriteria = "tcp_open_connections{direction=\"inbound\",peer=\"src\",tls=\"no_identity\",no_tls_reason=\"not_provided_by_remote\"}"
	}

	req, err := http.NewRequest(http.MethodGet, metricURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return stats, nil
	}
	defer resp.Body.Close()
	scannner := bufio.NewScanner(resp.Body)

	for scannner.Scan() {
		if strings.Contains(scannner.Text(), rqMatchCriteria) {
			parts := strings.Split(scannner.Text(), " ")
			if len(parts) == 2 {
				rqs, _ := strconv.Atoi(parts[1])
				totalRequest = int64(rqs)
			}
		}
		if strings.Contains(scannner.Text(), acrqMatchCriteria) {
			parts := strings.Split(scannner.Text(), " ")
			if len(parts) == 2 {
				rqs, _ := strconv.Atoi(parts[1])
				activeRequest = int64(rqs)
			}
		}
	}
	stats.AverageConcurrentRequests = float64(activeRequest*podCount)
	stats.RequestCount = int32(totalRequest*podCount - c.lastScrapedRequest[metricKey])
	c.lastScrapedRequest[metricKey] = totalRequest*podCount
	return stats, nil
}
