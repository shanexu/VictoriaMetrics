package main

import (
	"flag"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmselect/netstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmselect/prometheus"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmselect/promql"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/auth"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fs"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/procutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/timerpool"
	"github.com/VictoriaMetrics/metrics"
)

var (
	httpListenAddr        = flag.String("httpListenAddr", ":8481", "Address to listen for http connections")
	cacheDataPath         = flag.String("cacheDataPath", "", "Path to directory for cache files. Cache isn't saved if empty")
	maxConcurrentRequests = flag.Int("search.maxConcurrentRequests", runtime.GOMAXPROCS(-1)*2, "The maximum number of concurrent search requests. It shouldn't exceed 2*vCPUs for better performance. See also -search.maxQueueDuration")
	maxQueueDuration      = flag.Duration("search.maxQueueDuration", 10*time.Second, "The maximum time the request waits for execution when -search.maxConcurrentRequests limit is reached")
	storageNodes          = flagutil.NewArray("storageNode", "Addresses of vmstorage nodes; usage: -storageNode=vmstorage-host1:8401 -storageNode=vmstorage-host2:8401")
)

func main() {
	flag.Parse()
	buildinfo.Init()
	logger.Init()

	logger.Infof("starting netstorage at storageNodes=%s", *storageNodes)
	startTime := time.Now()
	if len(*storageNodes) == 0 {
		logger.Fatalf("storageNodes cannot be empty")
	}
	netstorage.InitStorageNodes(*storageNodes)
	logger.Infof("started netstorage in %s", time.Since(startTime))

	if len(*cacheDataPath) > 0 {
		tmpDataPath := *cacheDataPath + "/tmp"
		fs.RemoveDirContents(tmpDataPath)
		netstorage.InitTmpBlocksDir(tmpDataPath)
		promql.InitRollupResultCache(*cacheDataPath + "/rollupResult")
	} else {
		netstorage.InitTmpBlocksDir("")
		promql.InitRollupResultCache("")
	}
	concurrencyCh = make(chan struct{}, *maxConcurrentRequests)

	go func() {
		httpserver.Serve(*httpListenAddr, requestHandler)
	}()

	sig := procutil.WaitForSigterm()
	logger.Infof("service received signal %s", sig)

	logger.Infof("gracefully shutting down the service at %q", *httpListenAddr)
	startTime = time.Now()
	if err := httpserver.Stop(*httpListenAddr); err != nil {
		logger.Fatalf("cannot stop the service: %s", err)
	}
	logger.Infof("successfully shut down the service in %s", time.Since(startTime))

	logger.Infof("shutting down neststorage...")
	startTime = time.Now()
	netstorage.Stop()
	if len(*cacheDataPath) > 0 {
		promql.StopRollupResultCache()
	}
	logger.Infof("successfully stopped netstorage in %s", time.Since(startTime))

	logger.Infof("the vmselect has been stopped")
}

var concurrencyCh chan struct{}

func requestHandler(w http.ResponseWriter, r *http.Request) bool {
	// Limit the number of concurrent queries.
	// Sleep for a while until giving up. This should resolve short bursts in requests.
	t := timerpool.Get(*maxQueueDuration)
	select {
	case concurrencyCh <- struct{}{}:
		timerpool.Put(t)
		defer func() { <-concurrencyCh }()
	case <-t.C:
		timerpool.Put(t)
		httpserver.Errorf(w, "cannot handle more than %d concurrent requests", cap(concurrencyCh))
		return true
	}

	path := r.URL.Path
	if path == "/internal/resetRollupResultCache" {
		promql.ResetRollupResultCache()
		return true
	}

	p, err := httpserver.ParsePath(path)
	if err != nil {
		httpserver.Errorf(w, "cannot parse path %q: %s", path, err)
		return true
	}
	at, err := auth.NewToken(p.AuthToken)
	if err != nil {
		httpserver.Errorf(w, "auth error: %s", err)
		return true
	}
	switch p.Prefix {
	case "select":
		return selectHandler(w, r, p, at)
	case "delete":
		return deleteHandler(w, r, p, at)
	default:
		// This is not our link
		return false
	}
}

func selectHandler(w http.ResponseWriter, r *http.Request, p *httpserver.Path, at *auth.Token) bool {
	if strings.HasPrefix(p.Suffix, "prometheus/api/v1/label/") {
		s := p.Suffix[len("prometheus/api/v1/label/"):]
		if strings.HasSuffix(s, "/values") {
			labelValuesRequests.Inc()
			labelName := s[:len(s)-len("/values")]
			httpserver.EnableCORS(w, r)
			if err := prometheus.LabelValuesHandler(at, labelName, w, r); err != nil {
				labelValuesErrors.Inc()
				sendPrometheusError(w, r, err)
				return true
			}
			return true
		}
	}

	switch p.Suffix {
	case "prometheus/api/v1/query":
		queryRequests.Inc()
		httpserver.EnableCORS(w, r)
		if err := prometheus.QueryHandler(at, w, r); err != nil {
			queryErrors.Inc()
			sendPrometheusError(w, r, err)
			return true
		}
		return true
	case "prometheus/api/v1/query_range":
		queryRangeRequests.Inc()
		httpserver.EnableCORS(w, r)
		if err := prometheus.QueryRangeHandler(at, w, r); err != nil {
			queryRangeErrors.Inc()
			sendPrometheusError(w, r, err)
			return true
		}
		return true
	case "prometheus/api/v1/series":
		seriesRequests.Inc()
		httpserver.EnableCORS(w, r)
		if err := prometheus.SeriesHandler(at, w, r); err != nil {
			seriesErrors.Inc()
			sendPrometheusError(w, r, err)
			return true
		}
		return true
	case "prometheus/api/v1/series/count":
		seriesCountRequests.Inc()
		httpserver.EnableCORS(w, r)
		if err := prometheus.SeriesCountHandler(at, w, r); err != nil {
			seriesCountErrors.Inc()
			sendPrometheusError(w, r, err)
			return true
		}
		return true
	case "prometheus/api/v1/labels":
		labelsRequests.Inc()
		httpserver.EnableCORS(w, r)
		if err := prometheus.LabelsHandler(at, w, r); err != nil {
			labelsErrors.Inc()
			sendPrometheusError(w, r, err)
			return true
		}
		return true
	case "prometheus/api/v1/labels/count":
		labelsCountRequests.Inc()
		httpserver.EnableCORS(w, r)
		if err := prometheus.LabelsCountHandler(at, w, r); err != nil {
			labelsCountErrors.Inc()
			sendPrometheusError(w, r, err)
			return true
		}
		return true
	case "prometheus/api/v1/export":
		exportRequests.Inc()
		if err := prometheus.ExportHandler(at, w, r); err != nil {
			exportErrors.Inc()
			httpserver.Errorf(w, "error in %q: %s", r.URL.Path, err)
			return true
		}
		return true
	case "prometheus/federate":
		federateRequests.Inc()
		if err := prometheus.FederateHandler(at, w, r); err != nil {
			federateErrors.Inc()
			httpserver.Errorf(w, "error in %q: %s", r.URL.Path, err)
			return true
		}
		return true
	default:
		return false
	}
}

func deleteHandler(w http.ResponseWriter, r *http.Request, p *httpserver.Path, at *auth.Token) bool {
	switch p.Suffix {
	case "prometheus/api/v1/admin/tsdb/delete_series":
		deleteRequests.Inc()
		if err := prometheus.DeleteHandler(at, r); err != nil {
			deleteErrors.Inc()
			httpserver.Errorf(w, "error in %q: %s", r.URL.Path, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	default:
		return false
	}
}

func sendPrometheusError(w http.ResponseWriter, r *http.Request, err error) {
	logger.Errorf("error in %q: %s", r.URL.Path, err)

	w.Header().Set("Content-Type", "application/json")
	statusCode := 422
	w.WriteHeader(statusCode)
	prometheus.WriteErrorResponse(w, statusCode, err)
}

var (
	labelValuesRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/label/{}/values"}`)
	labelValuesErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="select/{}/prometheus/api/v1/label/{}/values"}`)

	queryRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/query"}`)
	queryErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/query"}`)

	queryRangeRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/prometheus/api/v1/query_range"}`)
	queryRangeErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/query_range"}`)

	seriesRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/series"}`)
	seriesErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/series"}`)

	seriesCountRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/series/count"}`)
	seriesCountErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/series/count"}`)

	labelsRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/labels"}`)
	labelsErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/labels"}`)

	labelsCountRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/labels/count"}`)
	labelsCountErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/labels/count"}`)

	deleteRequests = metrics.NewCounter(`vm_http_requests_total{path="/delete/{}/prometheus/api/v1/admin/tsdb/delete_series"}`)
	deleteErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/delete/{}/prometheus/api/v1/admin/tsdb/delete_series"}`)

	exportRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/api/v1/export"}`)
	exportErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/api/v1/export"}`)

	federateRequests = metrics.NewCounter(`vm_http_requests_total{path="/select/{}/prometheus/federate"}`)
	federateErrors   = metrics.NewCounter(`vm_http_request_errors_total{path="/select/{}/prometheus/federate"}`)
)
